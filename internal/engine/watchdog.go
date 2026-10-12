// 停滞看门狗（§3.9）与完整性终判（§3.11）。
//
// 看门狗要治的病：网络层疑似阻断（NAT 表清了、CDN 边缘挂了、Wi-Fi 静默
// 丢包）——worker 阻塞在无超时的 TCP 读上，界面冻结在 0 速几十分钟。
// 判据极简：监视线程每 200ms 读 done，45s 无前进即中止。中止必须
// **真掐**在飞请求（linked cancellation）——只设标志位的话 worker 要
// 等 TCP 自然超时（几分钟），界面假死（规格原文踩过）。
//
// 完整性终判要治的病：错误计数只是过程指标。某片失败记过 error 但数据
// 后来补齐，「存在过错误」≠「数据缺失」——实测 987MB 视频全盘无空洞
// 却被标失败，用户以为文件损坏（§3.11 依据原文）。
package engine

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// stalledEvery 停滞阈值生产默认（§3.9 逐字 now−lastAdvance ≥ 45s）。
// 注入点在 Watchdog.threshold（集成测试压成毫秒级）；200ms 节拍沿用
// Task.sampleEvery（监视线程驱动，精度要求见 §3.12 注）。
const stalledEvery = 45 * time.Second

// Watchdog 停滞看门狗（§3.9）：monitorA 每 200ms 调 Tick(now)（now 注入，
// 纯逻辑测试用假时钟）。规则逐字：
//
//	done 增长            → 刷新 lastAdvance
//	用户暂停中           → 刷新 lastAdvance（暂停是意愿不是故障）
//	now−lastAdvance ≥ 45s → 记 stalled 错误 + 看门狗级取消
//
// 「看门狗级取消」经 Task.fail 走 runA 的任务级 cancel——ctx 派生链
// （runA ctx → 片级 sctx → HTTP 请求）一刀到底，在飞读立即解除；
// finalizeCleanup 对 ErrStalled 保留断点（§3.10 表：环境问题可续传）。
// 收尾合并阶段（EnterMerge，模式 B Task 12 接线）不做停滞判定。
type Watchdog struct {
	t *Task // 读 done；触发时 fail（记错 + 级联取消）

	threshold time.Duration // 停滞阈值（生产 45s，测试注入）

	mu          sync.Mutex
	lastDone    int64     // 上一拍 done（增长即前进）
	lastAdvance time.Time // 上一前进时刻（零值 = 首拍锚定）
	stalled     bool      // 已触发（幂等）
	paused      bool      // 用户暂停中（刷新 lastAdvance，不判停滞）
	merging     bool      // 收尾合并阶段（§3.9：不判停滞；进度改报合并中）
}

// newWatchdog 构造看门狗（threshold 是唯一注入常数）。
func newWatchdog(t *Task, threshold time.Duration) *Watchdog {
	return &Watchdog{t: t, threshold: threshold}
}

// Tick 一拍判定（monitorA 每 sampleEvery 调用；now 注入）。触发动作
// （fail + 级联取消）在锁外执行——fail 内部拿 errMu/调 cancel，避免嵌套。
func (w *Watchdog) Tick(now time.Time) {
	w.mu.Lock()
	if w.stalled || w.merging {
		// 已触发（幂等）或合并阶段（只剩磁盘 I/O，永不判停滞）。
		w.mu.Unlock()
		return
	}
	if w.lastAdvance.IsZero() {
		w.lastAdvance = now // 首拍锚定基线（直接构造的测试路径免 Start）
	}
	done := w.t.done.Load()
	if done > w.lastDone || w.paused {
		// done 增长或用户暂停中 → 刷新（§3.9 逐字）。
		w.lastAdvance = now
	}
	w.lastDone = done
	idle := now.Sub(w.lastAdvance)
	w.mu.Unlock()

	if idle >= w.threshold {
		w.trigger(idle)
	}
}

// trigger 记 stalled 错误并执行看门狗级取消。fail 会记 firstErr（首个
// 非取消类致命错误）并调 Task.cancel——runA 的任务级 ctx 一断，worker
// 阻塞的读立即解除（真掐）；收尾按 §3.10 stalled 行保留断点。
func (w *Watchdog) trigger(idle time.Duration) {
	w.mu.Lock()
	already := w.stalled
	w.stalled = true
	w.mu.Unlock()
	if already {
		return
	}
	w.t.fail(fmt.Errorf("%w: %v 无前进（阈值 %s，中止但保留断点）",
		ErrStalled, idle.Round(time.Millisecond), w.threshold))
}

// SetPaused 登记/解除用户暂停（Manager/Task 14 或测试接线）：暂停期间
// 每拍刷新 lastAdvance——暂停是意愿不是故障，绝不误杀。
func (w *Watchdog) SetPaused(p bool) {
	w.mu.Lock()
	w.paused = p
	w.mu.Unlock()
}

// EnterMerge 进入收尾合并阶段（§3.9：worker 全结束、只剩磁盘 I/O——
// 模式 B 由 Task 12 接线；模式 A 永不进入）。此后 Tick 不再判定，进度
// 改报「正在合并分片」（Progress.Merging 状态位，合并吞吐归 Task 12）。
func (w *Watchdog) EnterMerge() {
	w.mu.Lock()
	w.merging = true
	w.mu.Unlock()
}

// Stalled 是否已触发停滞（监视线程/测试查询）。
func (w *Watchdog) Stalled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stalled
}

// Merging 是否处于收尾合并阶段（Progress.Merging 的数据源）。
func (w *Watchdog) Merging() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.merging
}

// Verdict 完整性终判（§3.11）结果。
type Verdict struct {
	Success     bool   // true = 数据完整，按成功处理（终态翻转 done、跳过清理删除）
	Done, Total int64  // 判据时刻的累计已写 / 总字节（日志与断言用）
	Note        string // 成功注记：有传输错误/取消但数据完整（出口：runA 翻转分支落 slog，F1）
}

// verdict 完整性终判（§3.11 逐字：报「失败」之前，先验证数据本身）。
// 仅在 cancelled 或 errors 非空时由 runA/runB 调用（成功路径无需终判）：
//
//	done ≥ total（模式 A）→ 按成功处理——错误计数只是过程指标，
//	「存在过错误」≠「数据缺失」（987MB 事故，§3.11 依据原文）。
//
// 模式 B（Task 12 已落地）：判据在 dataComplete 扩为「目标文件长度 ==
// total」复判（差一个字节都不算成功）。
func (t *Task) verdict() Verdict {
	done := t.done.Load()
	if t.dataComplete() {
		return Verdict{
			Success: true,
			Done:    done,
			Total:   t.Total,
			Note: fmt.Sprintf("有传输错误/取消但数据完整（done=%d ≥ total=%d），按成功处理",
				done, t.Total),
		}
	}
	return Verdict{Done: done, Total: t.Total}
}

// dataComplete 数据完整性判据（§3.11）。模式 A 逐字：done ≥ total。
// 模式 B（Task 12）：另加目标文件长度 == total 的复判——分片可能还躺在
// 分片目录里没合并完（记账满 ≠ 文件满），差一个字节都不算成功（§3.11
// 依据原文：模式 B 收尾还要再验一次，目标文件长度 ≠ total 差一个字节
// 也不能当成功）。
func (t *Task) dataComplete() bool {
	if t.done.Load() < t.Total {
		return false
	}
	if !t.modeB {
		return true
	}
	fi, err := os.Stat(t.Dest)
	return err == nil && fi.Size() == t.Total
}
