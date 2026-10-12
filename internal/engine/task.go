// Task：一个下载任务的运行时形态（§3.1 流水线 ⑤ 的执行主体）。
// 装配输入由 Task 14（Manager）或测试直接填写；未导出运行态字段随
// Task 8–12（重试/抢占/看门狗/模式 B）逐步累积，本文件只立骨架。
package engine

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// 任务状态机（骨架）：pending → running → done | error | canceled。
// atomic.Int32 存储，Progress.State 输出字符串。
type taskState int32

const (
	statePending taskState = iota
	stateRunning
	stateDone
	stateError
	stateCanceled
)

func (s taskState) String() string {
	switch s {
	case statePending:
		return "pending"
	case stateRunning:
		return "running"
	case stateDone:
		return "done"
	case stateError:
		return "error"
	case stateCanceled:
		return "canceled"
	}
	return "unknown"
}

// gate 是限流闸门（§3.8）：worker 领片前 Acquire、片处理完 Release。
//
// Ruling E1：本接口是 worker 与闸门之间的唯一契约——Task 7 的 runA 持有
// 未导出 gate 字段（默认 noOpGate 直通占位），Task 9 提供真实现（计数值
// 动态收敛）并扩展 Shrink/TickRecover 两个 §3.8 接线点，Task 14 装配。
// ctx 取消时 Acquire 必须返回错误让 worker 退出；Acquire 返回错误 ⇒
// 调用方不持票、不得 Release。
type gate interface {
	Acquire(ctx context.Context) error
	Release()
	Shrink()                   // §3.8：收到 429/503 时收缩（fetchSeg 错误分派处调用）
	TickRecover(now time.Time) // §3.8：监视线程周期调的恢复检查（now 注入便于测试）
}

// noOpGate 是闸门的直通占位：不限制并发，但尊重 ctx 生命周期；§3.8
// 接线点为空操作。
type noOpGate struct{}

func (noOpGate) Acquire(ctx context.Context) error { return ctx.Err() }
func (noOpGate) Release()                          {}
func (noOpGate) Shrink()                           {}
func (noOpGate) TickRecover(time.Time)             {}

// Progress 是一次进度快照（进度事件 §3.9 的载荷形态）。
type Progress struct {
	Done, Total int64   // 累计已写字节（= ΣW，进度只认记账 §3.6 ②）/ 文件总字节
	SpeedBPS    float64 // EWMA 速度估计（§3.12，字节/秒；首帧 0 属预期）
	State       string  // taskState 字符串
	Merging     bool    // §3.9 收尾合并阶段状态位：进度改报「正在合并分片」（合并吞吐归 Task 12）
}

// Task 是模式 A/B 下载任务的骨架结构。导出字段是装配输入（测试同包可直
// 接写未导出字段注入常数与钩子）；运行态由 runA/runB（Task 12）维护。
type Task struct {
	// ---- 装配输入 ----
	ID        string    // 任务标识（日志/事件路由用）
	Dest      string    // 目标文件路径；.qmeta 落在 Dest + ".qmeta"
	URL       string    // 下载直链
	CookieStr string    // 完整 Cookie 串（防盗链，§3.4）
	Threads   int       // 用户并发档位（N = min(Threads, 片数, 512)）
	Total     int64     // 文件总字节
	Segs      []Segment // 分片规划（Plan 产出；恢复对账后不变）
	W         []int64   // 每片已写字节（恢复时由 ReconcileA 结果填充）

	// ---- 可注入依赖（零值可用，newTask 填默认）----
	client *http.Client // 引擎客户端（§3.4：每次拨号刷新 DNS、禁代理）
	gate   gate         // Ruling E1：默认 noOpGate，Task 14 装配真闸门
	speed  *Speedometer

	// ---- 可注入常数（测试压成毫秒/微秒级；生产默认在 newTask）----
	ramp         time.Duration // 冷启动错峰总时长：sleep(ramp×w/N)（§3.5）
	sampleEvery  time.Duration // 监视线程速度采样节拍（§3.12：200ms）
	qmetaEvery   time.Duration // 监视线程 SaveQMeta 周期（§3.6：1s）
	preemptEvery time.Duration // 抢占扫描周期（§3.7：5s；监视线程内分频）

	// ---- Task 8（§3.10）----
	segMu     sync.Mutex                                 // segCancel 的锁
	segCancel map[int]context.CancelFunc                 // 片级取消令牌注册表（§3.5）：Task 10 抢占看门狗 cancelSeg(idx) 只掐本片
	sleep     func(context.Context, time.Duration) error // 片级重试退避等待（生产 sleepCtx；测试注入桩）

	// ---- Task 10（§3.7 慢连接抢占）----
	preempt *Preemptor // 两遍扫描看门狗（worker begin/end 登记、monitorA 周期 Scan；nil 防御：直接构造 Task 的路径）

	// ---- Task 11（§3.9 停滞看门狗）----
	watchdog *Watchdog // 停滞看门狗（monitorA 每 sampleEvery 节拍 Tick；nil 防御：直接构造 Task 的路径）

	// ---- 钩子（nil 安全；Task 14 / 测试装配）----
	onEvent       func(Progress) // 进度事件回调（监视线程节拍驱动）
	onWorkerStart func(w int)    // worker 冷启动完成钩子（错峰观测）

	// ---- 运行态 ----
	state    atomic.Int32       // taskState
	done     atomic.Int64       // ΣW：累计完成字节（进度只认它，§3.6 ②）
	cancel   context.CancelFunc // runA 派生的任务级取消（fail 时级联）
	wmu      sync.Mutex         // W 的快照锁：worker 逐片独占写、监视全量读
	errMu    sync.Mutex
	firstErr error // 首个非取消类致命错误（runA 返回值来源）
}

// newTask 按默认装配构造任务：分片规划、引擎客户端、直通闸门、生产常数
// （ramp=Ramp(N)、采样 200ms、qmeta 1s）。恢复续传时调用方自行 LoadQMeta
// + ReconcileA 后覆写 W 并把 ΣW 存入 done。
func newTask(id, dest, url, cookieStr string, threads int, total int64) *Task {
	segs := Plan(total, threads)
	n := max(min(threads, len(segs), effMax), 1)
	tk := &Task{
		ID:           id,
		Dest:         dest,
		URL:          url,
		CookieStr:    cookieStr,
		Threads:      threads,
		Total:        total,
		Segs:         segs,
		W:            make([]int64, len(segs)),
		client:       NewEngineClient(n),
		gate:         noOpGate{},
		speed:        NewSpeedometer(),
		ramp:         Ramp(n),                // N>256 ? 6s : 3s（§3.3）
		sampleEvery:  200 * time.Millisecond, // §3.12
		qmetaEvery:   time.Second,            // §3.6
		preemptEvery: preemptScanEvery,       // §3.7：5s
		sleep:        sleepCtx,               // §3.10 片级重试退避等待
	}
	tk.preempt = newPreemptor(tk, n, len(segs)) // §3.7 采样表按 worker 编号（n 与 runA 同式）
	tk.watchdog = newWatchdog(tk, stalledEvery) // §3.9 停滞阈值 45s（测试注入 tk.watchdog.threshold）
	return tk
}

// progress 返回当前进度快照。Done 与 State 原子读，SpeedBPS 内部有锁，
// 三者非同一瞬时（快照语义，进度展示足够）。
func (t *Task) progress() Progress {
	return Progress{
		Done:     t.done.Load(),
		Total:    t.Total,
		SpeedBPS: t.speed.BPS(),
		State:    taskState(t.state.Load()).String(),
		Merging:  t.watchdog != nil && t.watchdog.Merging(), // §3.9 合并阶段状态位
	}
}

// qmetaPath 返回 .qmeta 路径（Dest 同目录同名加后缀，§3.6）。
func (t *Task) qmetaPath() string { return t.Dest + ".qmeta" }

// saveMeta 全量快照 W 并原子落盘 .qmeta（§3.6 写入协议由 SaveQMeta 保证）。
func (t *Task) saveMeta() error {
	t.wmu.Lock()
	w := make([]int64, len(t.W))
	copy(w, t.W)
	t.wmu.Unlock()
	return SaveQMeta(t.qmetaPath(), &QMeta{Total: t.Total, W: w})
}

// fail 记录首个致命错误并级联取消其余 worker。取消类错误（外部 ctx
// canceled/deadline）不是失败：不记 firstErr，由 runA 末尾按 ctx.Err()
// 归入 canceled 终态。
func (t *Task) fail(err error) {
	if err == nil || isCancellation(err) {
		return
	}
	t.errMu.Lock()
	if t.firstErr == nil {
		t.firstErr = err
	}
	t.errMu.Unlock()
	if t.cancel != nil {
		t.cancel() // 级联：掐断其余 worker 在飞请求
	}
}

// isCancellation 判定 err 是否纯取消/超时（外部 ctx 生命周期结束）。
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
