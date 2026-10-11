// §3.10「重试与错误分类」：任务级错误类别（决定半成品生死）、片级
// 可重试判定、退避公式。哨兵错误供 Manager（Task 14）与看门狗/终判
// （Task 11）以 errors.Is 对接。
package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"
)

// 任务级错误哨兵（§3.10 错误分类表的行锚点）。
var (
	// ErrUserCancel 用户主动取消：外部取消令牌触发——半成品**删除**
	//（dest + .qmeta + 分片目录）。
	ErrUserCancel = errors.New("feiwu: 用户主动取消")

	// ErrStalled 停滞看门狗中止（§3.7，Task 11 触发）：环境问题，
	// 半成品**保留**（可续传）。
	ErrStalled = errors.New("feiwu: 停滞看门狗中止")

	// ErrRangeUnsupported Range 不支持（分片请求回 200）：分片写会从片
	// 偏移写坏整个文件，一个字节都不许落盘——半成品删除后转**单线程**
	// 全量下载（§3.5/§3.10）。
	ErrRangeUnsupported = errors.New("feiwu: 服务器忽略 Range（分片请求得到 200）")
)

// errPreempted 片被抢占（§3.7/Task 10）：片级取消令牌被掐而全局未取消。
// 不是失败——worker 收到后把片重新入队（不计失败、不清半成品、不退避）。
// 面向 Task 10 的可扩展点：抢占判定的「先判抢占再判取消」顺序框架在
// fetchSeg/workerA，看门狗只需 cancelSeg(idx)。
var errPreempted = errors.New("feiwu: 片被抢占")

// errShortBody 206 响应体在区间末尾之前提前结束（连接半途断开等）。
// §3.10 裁定（Task 8 重审）：纳入**片级可重试**——短体 = 连接中断的
// 可恢复形态，W 已逐块记账已写字节，重试从 S+W 断点续、不重复下载
// （§3.10「片级重试：每片独立」）；重试耗尽才升级为任务失败（见
// KindRetryExhausted）。Task 7 的致命终止语义由此废除。
var errShortBody = errors.New("feiwu: 片响应体短于请求区间")

// ErrorKind 任务级错误类别：决定半成品生死（§3.10 表）与 4xx 兜底判据。
type ErrorKind string

const (
	KindUserCancel       ErrorKind = "user-cancel"       // 用户主动取消 → 删
	KindStalled          ErrorKind = "stalled"           // 看门狗中止 → 保留
	KindRangeUnsupported ErrorKind = "range-unsupported" // 回 200 → 删后转单线程
	KindHTTP4xx          ErrorKind = "http-4xx"          // 确定性 4xx 拒绝（403/404…）→ 片不重试，任务失败后判兜底
	KindHTTPRetry        ErrorKind = "http-retryable"    // 408/416/429/5xx → 片级重试（§3.4/Task 7 交接）
	KindRetryExhausted   ErrorKind = "retry-exhausted"   // 片重试耗尽 → 见完整性终判（Task 11）
	KindFatal            ErrorKind = "fatal"             // 其它致命（磁盘满、预分配失败…）→ 保留（终判）
)

// TaskError 带类别的任务/片级错误。Status 是 HTTP 状态码（非 HTTP 错误
// 为 0）；兜底判据「失败原因属 4xx」沿 Cause 链下钻时按 Status 判定
// （416 属可重试类 KindHTTPRetry，但耗尽后仍是 4xx，须能被兜底认出）。
type TaskError struct {
	Kind   ErrorKind
	Status int
	Cause  error
	Detail string
}

func (e *TaskError) Error() string {
	if e == nil {
		return "<nil>"
	}
	s := "feiwu: " + string(e.Kind)
	if e.Status != 0 {
		s = fmt.Sprintf("%s(HTTP %d)", s, e.Status)
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

func (e *TaskError) Unwrap() error { return e.Cause }

// ClassifyError 把任意错误映射到任务级类别（半成品生死的输入）。
// 裸取消（context.Canceled/DeadlineExceeded）按用户取消归类——「其它
// 取消（反压等）→ 保留」的细分由 finalizeCleanup 以外部令牌状态判定，
// 不在本函数猜测。
func ClassifyError(err error) ErrorKind {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrStalled):
		return KindStalled
	case errors.Is(err, ErrUserCancel):
		return KindUserCancel
	case errors.Is(err, ErrRangeUnsupported):
		return KindRangeUnsupported
	}
	var te *TaskError
	if errors.As(err, &te) {
		return te.Kind
	}
	if isCancellation(err) {
		return KindUserCancel
	}
	return KindFatal
}

// retryableSeg 片级错误是否可重试（§3.10「片级重试：每片独立」的作用域）：
//   - 短体 / 网络错 / 408/416/429/5xx → 可重试（从 S+W 断点续）；
//   - 200 忽略 Range → 致命（分片写会写坏文件，一个字节都不许落盘）；
//   - 确定性 4xx（403/404…）→ 不重试：重试无意义，直接任务失败走
//     §3.10 的 4xx 零进度兜底判据；
//   - 取消类 → 不在本循环重试（抢占/取消由 fetchSeg 的顺序判定框架消化）。
func retryableSeg(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errPreempted) || isCancellation(err) {
		return false
	}
	if errors.Is(err, ErrRangeUnsupported) {
		return false
	}
	var te *TaskError
	if errors.As(err, &te) {
		switch te.Kind {
		case KindHTTP4xx:
			return false
		case KindHTTPRetry:
			return true
		}
	}
	return true // 网络错/短体等默认可重试
}

// httpKind 非期望状态码 → 类别（Task 7 交接的非 206 分化）：
// 408/429/5xx/416 可重试，其余 4xx 确定性拒绝。
func httpKind(status int) ErrorKind {
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests ||
		status == http.StatusRequestedRangeNotSatisfiable || status >= 500 {
		return KindHTTPRetry
	}
	if status >= 400 && status < 500 {
		return KindHTTP4xx
	}
	return KindHTTPRetry // 未定义的怪异状态：保守重试，耗尽终判兜底
}

// statusOf 从错误链挖第一个 TaskError 的 HTTP 状态码（非 HTTP 错误为
// 0）。重试耗尽包装（KindRetryExhausted）用它保留底层的 4xx 判据。
func statusOf(err error) int {
	var te *TaskError
	if errors.As(err, &te) {
		return te.Status
	}
	return 0
}

// causeIs4xx 兜底判据「任务失败原因属 4xx」（§3.10）：沿 Cause 链下钻
// 找 TaskError 的 Status。重试耗尽的包装（KindRetryExhausted）下底层
// 416/403 仍须被认出——416 属可重试类，耗尽后恰恰要靠兜底兜住。
func causeIs4xx(err error) bool {
	var te *TaskError
	for err != nil {
		if errors.As(err, &te) {
			if te.Status/100 == 4 {
				return true
			}
			err = te.Cause
			continue
		}
		err = errors.Unwrap(err)
	}
	return false
}

// segRetryMax 片级重试上限（Ruling E3）：重试**次数**（不含首次尝试）。
// §3.10「每片独立，最多 4 次**重试**」与 §7 常数表「片重试 ≤4 次」一致：
// 总尝试 = 1 + segRetryMax = 5 次（耗尽判定在第 5 次尝试失败后触发），
// 尝试间退避 BackoffSeg(0..3) 共 4 次（上界 500ms×8×1.4=5.6s）。
const segRetryMax = 4

// BackoffSeg 片级重试退避（§3.10 逐字）：500ms × 2^attempt × (0.6+0.8r)。
// r ∈ [0,1) 是随机源采样（worker 各持独立 rand 实例，§3.10「随机源」：
// 共享非线程安全实例会让抖动退化成同步退避，恰好促成重试风暴）。
// r=0 → ×0.6 下界，r→1 → ×1.4 上界开。纯函数；attempt<0 按 0 防御。
func BackoffSeg(attempt int, r float64) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if r < 0 {
		r = 0
	} else if r >= 1 {
		r = math.Nextafter(1, 0) // 保持 [0,1) 语义
	}
	jitter := 0.6 + 0.8*r
	return time.Duration(500 * float64(time.Millisecond) * math.Pow(2, float64(attempt)) * jitter)
}

// sleepCtx 真实退避等待：d 到点返回 nil；ctx 结束（抢占/取消）即刻
// 返回 ctx.Err()。Task.sleep 的生产默认值；测试注入桩替换。
func sleepCtx(ctx context.Context, d time.Duration) error {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tm.C:
		return nil
	}
}
