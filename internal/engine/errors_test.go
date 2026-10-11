// §3.10「重试与错误分类」纯逻辑测试：退避公式、TaskError 形态、
// 错误→类别映射、片级可重试判定、4xx 兜底判据的链式挖掘。
package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// BackoffSeg 公式值逐字：500ms × 2^attempt × (0.6+0.8r)，r∈[0,1)。
// r=0 → ×0.6（下界）；r=0.5 → ×1.0（中点）；r→1 → ×1.4（上界开）。
func TestBackoffSeg(t *testing.T) {
	base := 500 * float64(time.Millisecond)
	cases := []struct {
		attempt int
		r       float64
		mul     float64 // 期望的抖动系数
	}{
		{0, 0, 0.6},
		{0, 0.5, 1.0},
		{0, 0.999999, 0.6 + 0.8*0.999999},
		{1, 0, 0.6},
		{1, 0.5, 1.0},
		{2, 0.5, 1.0},
		{3, 0.5, 1.0},
		{3, 0, 0.6},
	}
	for _, c := range cases {
		want := time.Duration(base * math.Pow(2, float64(c.attempt)) * c.mul)
		got := BackoffSeg(c.attempt, c.r)
		if got != want {
			t.Errorf("BackoffSeg(%d, %v) = %v, want %v（500ms×2^%d×%v）",
				c.attempt, c.r, got, want, c.attempt, c.mul)
		}
	}
	// 每档 attempt 的合法抖动区间（worker 实际调用用随机 r，值必落此区间）：
	// attempt=0 → [300ms, 700ms)；attempt=3 → [2.4s, 5.6s)
	if lo, hi := BackoffSeg(0, 0), BackoffSeg(0, 0.999999); lo != 300*time.Millisecond || hi >= 700*time.Millisecond {
		t.Errorf("attempt=0 抖动区间 = [%v, %v), want [300ms, 700ms)", lo, hi)
	}
	if lo := BackoffSeg(3, 0); lo != 2400*time.Millisecond {
		t.Errorf("BackoffSeg(3, 0) = %v, want 2.4s", lo)
	}
	// 防御：负 attempt 视为 0（纯函数对非法输入不 panic）
	if BackoffSeg(-1, 0.5) != BackoffSeg(0, 0.5) {
		t.Errorf("负 attempt 应按 0 处理")
	}
}

// TaskError：Error() 文案含 kind/状态码/细节/原因；Unwrap 暴露 Cause
// （errors.Is/As 沿链工作）。
func TestTaskError(t *testing.T) {
	inner := errors.New("底层错")
	te := &TaskError{Kind: KindRetryExhausted, Status: 416, Cause: inner, Detail: "片 3"}
	if !errors.Is(te, inner) {
		t.Fatalf("errors.Is 应沿 Unwrap 命中 Cause")
	}
	var as *TaskError
	if !errors.As(fmt.Errorf("外层: %w", te), &as) || as != te {
		t.Fatalf("errors.As 应穿透 fmt 包装命中 TaskError")
	}
	msg := te.Error()
	for _, want := range []string{string(KindRetryExhausted), "416", "片 3", "底层错"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, 应包含 %q", msg, want)
		}
	}
}

// ClassifyError：错误 → 任务级类别（决定半成品生死的输入）。
func TestClassifyError(t *testing.T) {
	cases := []struct {
		err  error
		want ErrorKind
	}{
		{context.Canceled, KindUserCancel},
		{context.DeadlineExceeded, KindUserCancel},
		{fmt.Errorf("包装: %w", context.Canceled), KindUserCancel},
		{ErrStalled, KindStalled},
		{fmt.Errorf("看门狗: %w", ErrStalled), KindStalled},
		{ErrRangeUnsupported, KindRangeUnsupported},
		{&TaskError{Kind: KindRangeUnsupported, Cause: ErrRangeUnsupported}, KindRangeUnsupported},
		{&TaskError{Kind: KindHTTP4xx, Status: 403}, KindHTTP4xx},
		{&TaskError{Kind: KindRetryExhausted, Cause: errShortBody}, KindRetryExhausted},
		{fmt.Errorf("片 2 写入失败: %w", errShortBody), KindFatal},
		{errors.New("任意"), KindFatal},
	}
	for _, c := range cases {
		if got := ClassifyError(c.err); got != c.want {
			t.Errorf("ClassifyError(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	// 重试耗尽包装下钻：Kind 取最外层 TaskError（耗尽），不误取 Cause 的类别
	wrapped := fmt.Errorf("任务失败: %w", &TaskError{Kind: KindRetryExhausted, Cause: &TaskError{Kind: KindHTTPRetry, Status: 416}})
	if got := ClassifyError(wrapped); got != KindRetryExhausted {
		t.Errorf("ClassifyError(耗尽包装) = %q, want %q", got, KindRetryExhausted)
	}
}

// retryableSeg：片级错误是否可重试（§3.10「片级重试」的作用域）。
// 关键裁定：errShortBody 从 Task 7 的致命改为可重试——短体=连接中断的
// 可恢复形态，W 已记录已写字节，重试从 S+W 断点续，不重复下载；
// 重试耗尽才任务失败（§3.10「每片独立」）。
func TestRetryableSeg(t *testing.T) {
	cases := []struct {
		err  error
		want bool
		note string
	}{
		{fmt.Errorf("片 0: %w（要 100 得 50）", errShortBody), true, "短体可重试（断点续）"},
		{&TaskError{Kind: KindHTTPRetry, Status: 416}, true, "416 可重试（Task 7 交接：瞬时 Range 异常）"},
		{&TaskError{Kind: KindHTTPRetry, Status: 500}, true, "5xx 可重试"},
		{&TaskError{Kind: KindHTTPRetry, Status: 408}, true, "408 可重试（§3.4）"},
		{&TaskError{Kind: KindHTTPRetry, Status: 429}, true, "429 可重试（§3.4）"},
		{errors.New("dial tcp: connection refused"), true, "网络错默认可重试"},
		{&TaskError{Kind: KindRangeUnsupported, Cause: ErrRangeUnsupported}, false, "200 忽略 Range 致命（分片写会写坏文件）"},
		{&TaskError{Kind: KindHTTP4xx, Status: 403}, false, "403 确定性拒绝不重试（走任务级 4xx 兜底）"},
		{&TaskError{Kind: KindHTTP4xx, Status: 404}, false, "404 不重试"},
		{context.Canceled, false, "取消不重试（按取消归类）"},
		{fmt.Errorf("get: %w", context.Canceled), false, "包装的取消同样不重试"},
		{errPreempted, false, "抢占不是错误输入（防御）"},
	}
	for _, c := range cases {
		if got := retryableSeg(c.err); got != c.want {
			t.Errorf("retryableSeg(%v) = %v, want %v（%s）", c.err, got, c.want, c.note)
		}
	}
}

// causeIs4xx：兜底判据「任务失败原因属 4xx」要沿 Cause 链下钻——
// 重试耗尽的包装下，底层 416/403 仍须被认出（416 属可重试类，耗尽后
// 恰恰要靠兜底兜住）。
func TestCauseIs4xx(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&TaskError{Kind: KindHTTP4xx, Status: 403}, true},
		{&TaskError{Kind: KindHTTPRetry, Status: 416}, true},
		{&TaskError{Kind: KindRetryExhausted, Status: 416, Cause: &TaskError{Kind: KindHTTPRetry, Status: 416}}, true},
		{&TaskError{Kind: KindRetryExhausted, Cause: fmt.Errorf("片 0: %w（要 100 得 50）", errShortBody)}, false},
		{&TaskError{Kind: KindHTTPRetry, Status: 503}, false},
		{errShortBody, false},
		{context.Canceled, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := causeIs4xx(c.err); got != c.want {
			t.Errorf("causeIs4xx(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
