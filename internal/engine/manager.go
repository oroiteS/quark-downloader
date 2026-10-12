// Manager（Task 14 收官）：任务装配、生命周期与直链自愈。
//
// 装配拓扑（§3.1 流水线的运行时宿主）：
//
//	Submit → TryLockDest（§3.13 同路径互斥）→ 排队（任务级信号量）→
//	  buildAttempt（newTask + .qmeta 恢复对账 + NewGate 真闸门 + 节流事件）→
//	  runA/runB（worker 全家桶）→ 终态事件 + UnlockDest
//
// 直链自愈（§2.10 任务级）：整体尝试失败且 401/403/412 → 刷新次数 <5 →
// Refresher 回调换新 URL → 新 URL 与旧相同或取不到 → 中止保留断点；
// 否则新 URL 重跑整个下载尝试（buildAttempt 从 .qmeta 读回，无损续传，
// 设计 G4）。刷新在 Manager 的任务循环里**一次失败换一次链**——绝不能
// 让每个 worker 各自刷新，否则一次直链失效会触发几百次重复取链调用
// （§2.10 事故原文）。
//
// 信号量说明：不用 Scheduler.Go 而由 Manager 自持 sem channel——Go 有三处
// 局限（T13 交接）：① ctx 固定 Background，排队期取消摸不到；② runX 返回
// 值丢弃（T7-F1：装配期错误如 Threads<1 只能靠返回值暴露，Manager 必须
// 接收并发错误事件）；③ 自愈重跑需要回到循环顶换 URL 再跑。Scheduler 的
// TryLockDest/UnlockDest（owned 语义）照常消费；改 Scheduler 签名的方案
// 留给后续任务（改动最小原则）。
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quark-downloader/internal/protocol"
)

// Mode 落盘模式（§3.5）：ModeA 直写目标文件；ModeB 独立分片 + 边下边合并。
type Mode int

const (
	ModeA Mode = iota
	ModeB
)

// Config Manager 装配配置（设计 G5：零值可用）。
// 零值默认在 NewManager 规范化：Threads 32 / ModeA / 3 任务。
type Config struct {
	Threads            int    // 单文件并发档位（多任务预算分配 AllocateBudget 归上层 Web API，本层单任务直用）
	Mode               Mode   // 默认落盘模式（Submit 的 mode 参数逐文件生效）
	MaxConcurrentTasks int    // 同时在跑的任务数上限
	DestDir            string // 下载根目录；dest = DestDir + relDir + FileName（G11 一文件一任务）
}

const defaultThreads = 32 // Config 零值默认（G5；与 §3.13 默认并发 3 对齐见 defaultMaxConcurrent）

// eventEveryDefault 进度事件节流（Task 14：500ms；Task 采样 200ms 不变）。
const eventEveryDefault = 500 * time.Millisecond

// defaultMaxTasks 登记表条目上限默认（F2 审查）：终态淘汰门槛。登记条目
// 常驻 Cookie/URL 等上下文（百字节级），1024 条 ≈ 数百 KB；超限按终态
// 时间最早淘汰**已终态**条目（存活任务绝不淘汰）。
const defaultMaxTasks = 1024

// refreshMax 直链自愈刷新上限（§2.10 逐字「刷新次数 < 5」：直链 TTL 约
// 5~10 分钟，9GB 任务约碰 2~4 次，上限防死循环）。
const refreshMax = 5

// defaultRefreshTimeout 刷新调用超时（F1 审查）：规格未定刷新时长——
// refresher 实为秒级协议调用（带 Cookie 重取直链），60s 只是兜底工程
// 选择；该层与任务取消链任一到期，都按「取不到新链」中止保留断点。
const defaultRefreshTimeout = 60 * time.Second

// ErrPaused 用户暂停（Manager 语义）：Pause 经 Task.fail 注入，runA/runB
// 以它收尾（isCancellation=false → finalizeCleanup 保留半成品），Manager
// 收到后不发终态、任务挂起等 Resume（buildAttempt 从 .qmeta 无损续传）。
var ErrPaused = errors.New("feiwu: 用户暂停（断点保留）")

// ErrTaskNotFound Get/控制接口的任务不存在错误。
var ErrTaskNotFound = errors.New("feiwu: 任务不存在")

// ErrClosed Manager 已 Close：Submit 拒绝新任务（F2 审查）。
var ErrClosed = errors.New("feiwu: Manager 已关闭")

// Event 任务事件：进度帧（节流 500ms）与终态帧（Done 单调不减）。
type Event struct {
	TaskID   string
	Progress Progress
	Err      error // 终态错误帧非 nil（用户取消为 ErrUserCancel 包装）
}

// SubmitOpt Submit 可选注入（零值全空 = 无自愈，401/403/412 直接失败）。
type SubmitOpt struct {
	// Refresher 重取直链回调（§2.10：需要保存 fid/分享上下文才能重取，
	// 由上层注入）。ctx 由 Manager 提供——挂在该任务取消链上（Cancel 可
	// 达）并带 60s 超时，回调应尊重 ctx；两层任一到期、返回错误、返回
	// 空串或与旧相同的 URL，都按「取不到新链」中止保留断点。
	Refresher func(ctx context.Context) (string, error)
}

// taskPhase 任务生命周期：queued → running →（paused ⇄ running）→ terminal。
type taskPhase int

const (
	phaseQueued   taskPhase = iota // 已提交（排队中或起跑前）
	phaseRunning                   // 某 attempt 在跑
	phasePaused                    // 暂停挂起（goroutine 已退，等 Resume 重新排队）
	phaseTerminal                  // 终态：done / error / canceled
)

// managedTask Manager 侧的任务登记（Task 是单次 attempt 的运行时形态，
// 自愈换链/暂停续传都换新 Task；managedTask 承载跨 attempt 的生命周期）。
type managedTask struct {
	id        string
	dest      string
	absLower  string // TryLockDest 登记键（绝对路径小写）
	lr        protocol.LinkResult
	cookie    string
	mode      Mode
	refresher func(ctx context.Context) (string, error)

	mu        sync.Mutex
	task      *Task              // 当前/最后 attempt（Get 读进度、Cancel/Retry 清理）
	cancel    context.CancelFunc // 当前 loop 的取消链根 cancel（nil=未起跑/已收尾）
	phase     taskPhase
	terminal  string // phaseTerminal 时的终态字符串
	termErr   error
	termAt    time.Time // 终态时刻（F2 淘汰排序键）
	refreshes int       // §2.10 刷新计数（<5）
	paused    bool      // 暂停意愿（loop 循环顶与 Pause/Resume 读写）
	canceled  bool      // 取消意愿（排队期/刷新窗口也生效——Scheduler 无 ctx 的补偿，T13）

	th eventThrottle // 进度事件节流器（跨 attempt 共享，防切换瞬间双发）
}

// eventThrottle 进度事件节流（500ms）：last 是上次发帧时刻（零值=未发过，
// 首帧立即发——UI 快速反馈）。
type eventThrottle struct {
	mu   sync.Mutex
	last time.Time
}

// Manager 任务管理器：装配 Config/事件/Scheduler 互斥，宿主任务循环。
type Manager struct {
	cfg            Config
	onEvent        func(Event)
	eventEvery     time.Duration
	refreshTimeout time.Duration // 刷新调用超时（默认 defaultRefreshTimeout，测试可注入）

	sched  *Scheduler    // TryLockDest/UnlockDest（owned 语义，§3.13）
	sem    chan struct{} // 任务级信号量（自持，原因见文件头「信号量说明」）
	tasks  map[string]*managedTask
	mu     sync.Mutex
	nextID atomic.Int64

	closed   bool           // Close 后为真：Submit 拒绝、Resume/Retry 不复活（F2）
	wg       sync.WaitGroup // 任务 goroutine（loop）计数：Close 等齐退出（F2）
	maxTasks int            // 登记表上限（默认 1024；未导出，测试可注入，F2）

	// taskTune 测试注入：压缩任务常数（ramp/采样/存档周期）。生产 nil。
	taskTune func(*Task)
}

// NewManager 构造管理器。onEvent 可为 nil（不发事件，Get 仍可用）；
// 回调可能在多个 goroutine 并发调用（监视线程发进度帧、任务循环发终态
// 帧），调用方自行保证线程安全。Config 零值规范化：Threads<1→32、
// MaxConcurrentTasks<1→3（设计 G5）。
func NewManager(cfg Config, onEvent func(Event)) *Manager {
	if cfg.Threads < 1 {
		cfg.Threads = defaultThreads
	}
	if cfg.MaxConcurrentTasks < 1 {
		cfg.MaxConcurrentTasks = defaultMaxConcurrent
	}
	return &Manager{
		cfg:            cfg,
		onEvent:        onEvent,
		eventEvery:     eventEveryDefault,
		refreshTimeout: defaultRefreshTimeout,
		sched:          NewScheduler(cfg.MaxConcurrentTasks),
		sem:            make(chan struct{}, cfg.MaxConcurrentTasks),
		tasks:          make(map[string]*managedTask),
		maxTasks:       defaultMaxTasks,
	}
}

// Submit 提交下载任务（一文件一任务，G11）。dest = DestDir + relDir +
// FileName（relDir 相对目录落盘，拒绝 .. 逃逸）；同路径互斥（TryLockDest
// 失败 → 「同一文件正在下载中」，不排队——两个下载器交叉写同一文件会
// 损坏数据，§3.13）。mode 逐文件生效；opts 注入 Refresher（§2.10 自愈，
// 缺省 401/403/412 直接失败）。
func (m *Manager) Submit(lr protocol.LinkResult, relDir, cookieStr string, mode Mode, opts ...SubmitOpt) (string, error) {
	if m.isClosed() {
		return "", ErrClosed
	}
	if lr.FileName == "" {
		return "", errors.New("feiwu: Submit: FileName 为空")
	}
	if lr.URL == "" {
		return "", errors.New("feiwu: Submit: URL 为空")
	}
	if lr.Size <= 0 {
		return "", fmt.Errorf("feiwu: Submit: Size=%d 非法（须 >0）", lr.Size)
	}
	if mode != ModeA && mode != ModeB {
		return "", fmt.Errorf("feiwu: Submit: 未知模式 %d", int(mode))
	}
	if clean := filepath.Clean(relDir); clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("feiwu: Submit: relDir %q 逃逸 DestDir", relDir)
	}
	dest := filepath.Join(m.cfg.DestDir, relDir, lr.FileName)
	abs, err := filepath.Abs(dest)
	if err != nil {
		return "", fmt.Errorf("feiwu: Submit: 解析目标路径失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("feiwu: Submit: 创建目标目录失败: %w", err)
	}
	absLower := strings.ToLower(abs)
	if !m.sched.TryLockDest(absLower) {
		return "", fmt.Errorf("feiwu: 同一文件正在下载中: %s", dest)
	}

	var refresher func(ctx context.Context) (string, error)
	for _, o := range opts {
		if o.Refresher != nil {
			refresher = o.Refresher
		}
	}
	id := fmt.Sprintf("t%d", m.nextID.Add(1))
	mt := &managedTask{
		id:        id,
		dest:      dest,
		absLower:  absLower,
		lr:        lr,
		cookie:    cookieStr,
		mode:      mode,
		refresher: refresher,
		phase:     phaseQueued,
	}
	mt.task = m.buildAttempt(mt, lr.URL) // 排队期 Get 可报 pending/已有断点的进度
	m.mu.Lock()
	if m.closed { // 注册临界区内复查（Close 可能与 Submit 并发；F2）
		m.mu.Unlock()
		m.sched.UnlockDest(absLower, true) // owned：Submit 成功才持有，失败路径手动归还
		return "", ErrClosed
	}
	m.tasks[id] = mt
	m.evictLocked() // F2：注册后过限先淘汰终态（新条目必存活）
	m.mu.Unlock()

	m.wg.Add(1) // 先 Add 后 go：Close 的 wg.Wait 不漏计（F2）
	go m.loop(mt)
	return id, nil
}

// buildAttempt 装配单次尝试的 Task（§3.1 流水线 ①②③）：
// newTask（Plan + 引擎客户端 + 生产常数）→ .qmeta 恢复对账（§3.6/§2.10
// 无损续传——自愈换链与暂停续传都从这里读回 W）→ NewGate 真闸门（Ruling
// E1：替换 newTask 的 noOpGate 占位）→ 节流事件钩子。
func (m *Manager) buildAttempt(mt *managedTask, url string) *Task {
	tk := newTask(mt.id, mt.dest, url, mt.cookie, m.cfg.Threads, mt.lr.Size)
	if mt.mode == ModeB {
		tk.modeB = true
		tk.partsDir = mt.dest + ".qparts"
	}
	if qm, err := LoadQMeta(tk.qmetaPath()); err == nil {
		if mt.mode == ModeB {
			if w, merged, inHead, ok := ReconcileB(mt.dest, tk.partsDir, qm, tk.Segs); ok {
				tk.W = w
				tk.mergeNext, tk.mergeInPart = merged, inHead
				tk.done.Store(sumW(w))
			}
		} else if w, ok := ReconcileA(mt.dest, qm, tk.Segs); ok {
			tk.W = w
			tk.done.Store(sumW(w))
		}
		// 对账不匹配（total/划分变了）→ 丢弃元数据全新开始（§3.6）。
	}
	n := max(min(tk.Threads, len(tk.Segs), effMax), 1)
	tk.gate = NewGate(n)
	tk.onEvent = m.progressHook(mt)
	if m.taskTune != nil {
		m.taskTune(tk)
	}
	return tk
}

// progressHook 返回 Task.onEvent 的节流包装（500ms；Task 采样 200ms 不变，
// 高频采样喂速度计、低频节流出帧）。终态/暂停帧由 Manager 直发并刷新
// 节流锚（emitFinal/touchThrottle）。
func (m *Manager) progressHook(mt *managedTask) func(Progress) {
	return func(p Progress) {
		if m.onEvent == nil {
			return
		}
		mt.th.mu.Lock()
		now := time.Now()
		if !mt.th.last.IsZero() && now.Sub(mt.th.last) < m.eventEvery {
			mt.th.mu.Unlock()
			return
		}
		mt.th.last = now
		mt.th.mu.Unlock()
		m.onEvent(Event{TaskID: mt.id, Progress: p})
	}
}

// touchThrottle 刷新节流锚（终态/暂停直发帧后调用，压制后续迟到帧）。
func (m *Manager) touchThrottle(mt *managedTask) {
	mt.th.mu.Lock()
	mt.th.last = time.Now()
	mt.th.mu.Unlock()
}

// emitFinal 直发一帧（终态/暂停；不走节流——终态必须到达，Done 取当前
// Task 计账保证单调）。
func (m *Manager) emitFinal(mt *managedTask, state string, err error) {
	m.touchThrottle(mt)
	if m.onEvent == nil {
		return
	}
	mt.mu.Lock()
	p := Progress{}
	if mt.task != nil {
		p = mt.task.progress()
	}
	mt.mu.Unlock()
	p.State = state
	p.Merging = false
	m.onEvent(Event{TaskID: mt.id, Progress: p, Err: err})
}

// loop 任务主循环（每个生命周期一段 goroutine：Submit/Resume/Retry 各起
// 一段；暂停挂起即退出让出信号量，Resume 重新排队）。占槽后在循环顶
// 消化取消/暂停意愿（T13：Scheduler 无 ctx，排队期取消靠标志表达——
// 起跑前检查跳过）；自愈重跑在**同一槽位内**回到循环顶（换链不算新任务，
// 不重新排队）。
func (m *Manager) loop(mt *managedTask) {
	defer m.wg.Done() // F2：Close 等齐 goroutine 的计数（Add 先于 go loop）
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	// F1（审查）：任务级取消链根。一次 loop 生命周期（跨自愈换链的多次
	// attempt）：runX 的 attempt ctx 与 Refresher 的 ctx 都从这里派生——
	// Cancel 经 mt.cancel 掐根，刷新窗口内同样可达（旧实现刷新用
	// context.Background 且无超时，回调挂起 = 任务 goroutine 永久阻塞且
	// 持有 sem 槽位，Cancel 掐不到）。
	runCtx, runCancel := context.WithCancel(context.Background())
	mt.setCancel(runCancel)
	defer func() { runCancel(); mt.setCancel(nil) }()

	url := mt.lr.URL
	for {
		if mt.isCanceled() {
			m.finishCanceled(mt)
			return
		}
		if mt.isPaused() {
			m.suspend(mt)
			return
		}

		tk := m.buildAttempt(mt, url)
		mt.setRunning(tk, url)
		ctx, cancel := context.WithCancel(runCtx) // attempt 从任务根派生（用户取消 = 外部令牌语义不变）

		var err error
		if mt.mode == ModeB {
			err = tk.runB(ctx) // T7-F1：返回值必收（装配期错误在此暴露）
		} else {
			err = tk.runA(ctx)
		}
		cancel()
		// 此处不 setCancel(nil)：mt.cancel 指向 runCancel——刷新窗口内
		// Cancel 仍可达（见上 F1）。

		switch {
		case err == nil:
			m.finish(mt, "done", nil)
			return
		case errors.Is(err, ErrPaused):
			m.suspend(mt)
			return
		case isCancellation(err), mt.isCanceled():
			// 用户取消：finalizeCleanup 已按外部令牌语义删除半成品。
			m.finish(mt, "canceled", err)
			return
		}
		if m.maybeRefresh(runCtx, mt, &url, err) {
			continue // 新 URL 重跑整个下载尝试；W 从 .qmeta 读回无损续传
		}
		m.finish(mt, "error", err)
		return
	}
}

// maybeRefresh §2.10 直链自愈判定（任务级一次一换）：整体尝试失败且
// 401/403/412 → 刷新次数 <5 → Refresher 换新 URL。取不到（错误/空串）或
// 与旧相同 → false（调用方中止保留断点）；换到新链 → true（重跑）。
// **绝不在此之外的任何层刷新**——worker 各自刷新 = 一次失效几百次取链。
// runCtx 是 loop 建的任务级取消链根（F1）：refresher 的 ctx 从它派生
// （Cancel 可达）再包一层工程超时；两层任一到期都按取不到新链处理。
func (m *Manager) maybeRefresh(runCtx context.Context, mt *managedTask, url *string, err error) bool {
	if !causeStatusIn(err, http.StatusUnauthorized, http.StatusForbidden, http.StatusPreconditionFailed) {
		return false
	}
	mt.mu.Lock()
	refresher, used := mt.refresher, mt.refreshes
	mt.mu.Unlock()
	if refresher == nil || used >= refreshMax {
		return false
	}
	rctx, rcancel := context.WithTimeout(runCtx, m.refreshTimeout)
	newURL, rerr := refresher(rctx)
	rcancel()
	mt.mu.Lock()
	mt.refreshes++
	mt.mu.Unlock()
	if rerr != nil || newURL == "" || newURL == *url {
		return false
	}
	*url = newURL
	slog.Info("feiwu: 直链自愈换链重跑", "task", mt.id, "refreshes", mt.refreshes, "dest", mt.dest)
	return true
}

// finish 终态收尾：释放同路径登记 → 记终态 → 直发终态帧。顺序保证
// Get 观察到终态时锁必已释放（Retry/新 Submit 立即可复用该路径）。
// 新终态可能把登记表推过上限（此前全存活顶满时）→ 触发终态淘汰（F2）。
func (m *Manager) finish(mt *managedTask, state string, err error) {
	m.sched.UnlockDest(mt.absLower, true) // owned：Submit 成功即持有
	mt.setTerminal(state, err)
	m.emitFinal(mt, state, err)
	m.mu.Lock()
	m.evictLocked()
	m.mu.Unlock()
}

// finishCanceled 排队期/刷新窗口/挂起期的取消收尾（runX 不在跑，
// finalizeCleanup 的删除语义由 Manager 兜底执行）。
func (m *Manager) finishCanceled(mt *managedTask) {
	mt.mu.Lock()
	tk := mt.task
	mt.mu.Unlock()
	if tk != nil {
		_ = tk.removePartial()
	}
	m.finish(mt, "canceled", context.Canceled)
}

// suspend 暂停挂起：goroutine 退出（让出信号量），dest 登记保持到终态
// （暂停期间同路径仍互斥），等 Resume 重新排队。
func (m *Manager) suspend(mt *managedTask) {
	mt.setPhase(phasePaused)
	m.emitFinal(mt, "paused", nil)
}

// Pause 暂停（T11 接线：暂停=意愿，看门狗不判停滞）。经 Task.fail 注入
// ErrPaused → runX 收尾保留断点（非用户取消、不删半成品）；挂起后进度
// 冻结（监视线程已随 attempt 退出）。排队期暂停：起跑前检查挂起。
func (m *Manager) Pause(id string) {
	mt := m.lookup(id)
	if mt == nil {
		return
	}
	mt.mu.Lock()
	if mt.phase != phaseRunning && mt.phase != phaseQueued {
		mt.mu.Unlock()
		return
	}
	mt.paused = true
	tk := mt.task
	running := mt.phase == phaseRunning
	mt.mu.Unlock()
	if running && tk != nil {
		if tk.watchdog != nil {
			tk.watchdog.SetPaused(true) // 收尾窗口内也不判停滞（暂停是意愿不是故障）
		}
		tk.fail(ErrPaused) // 记 firstErr + 级联取消在飞请求；收尾走「保留」分支
	}
}

// Resume 恢复：清暂停意愿 → 重新排队（buildAttempt 从 .qmeta 无损续传）。
func (m *Manager) Resume(id string) {
	if m.isClosed() { // F2：Close 后不复活（复活职责在 web 层 §6.2 持久化）
		return
	}
	mt := m.lookup(id)
	if mt == nil {
		return
	}
	mt.mu.Lock()
	if mt.phase != phasePaused {
		mt.mu.Unlock()
		return
	}
	mt.paused = false
	mt.phase = phaseQueued
	tk := mt.task
	mt.mu.Unlock()
	if tk != nil && tk.watchdog != nil {
		tk.watchdog.SetPaused(false) // T11 接线（对旧 attempt 幂等无害）
	}
	m.wg.Add(1)
	go m.loop(mt)
}

// Cancel 取消（用户意愿，§3.10 表：删半成品）。运行期：掐当前 attempt
// 的父 ctx → finalizeCleanup 删除；排队期：设标志，起跑前跳过（T13）；
// 挂起期：goroutine 已退，Manager 亲自删（finishCanceled）。
func (m *Manager) Cancel(id string) {
	mt := m.lookup(id)
	if mt == nil {
		return
	}
	mt.mu.Lock()
	if mt.phase == phaseTerminal {
		mt.mu.Unlock()
		return
	}
	mt.canceled = true
	cancel, phase := mt.cancel, mt.phase
	mt.mu.Unlock()

	switch phase {
	case phaseRunning, phaseQueued:
		if cancel != nil {
			cancel() // 外部令牌 → 用户取消删除语义（finalizeCleanup ②）
		}
	case phasePaused:
		m.finishCanceled(mt)
	}
}

// Retry 重试：error/canceled/done 终态或 paused 挂起态复活（error 态断点
// 保留在盘，buildAttempt 无损续传；canceled/done 态盘上已无断点则全新
// 开始）。运行中/排队中无效（记日志）。终态重试需重新 TryLockDest
// （路径可能已被其他任务占用）。
func (m *Manager) Retry(id string) {
	if m.isClosed() { // F2：Close 后不复活（复活职责在 web 层 §6.2 持久化）
		return
	}
	mt := m.lookup(id)
	if mt == nil {
		return
	}
	mt.mu.Lock()
	switch mt.phase {
	case phaseQueued, phaseRunning:
		mt.mu.Unlock()
		slog.Warn("feiwu: Retry 对运行中/排队中任务无效", "task", id)
		return
	case phasePaused:
		mt.paused = false
	case phaseTerminal:
		if !m.sched.TryLockDest(mt.absLower) {
			mt.mu.Unlock()
			slog.Warn("feiwu: Retry 失败——同一文件正在下载中", "task", id)
			return
		}
	}
	mt.canceled = false
	mt.refreshes = 0
	mt.phase = phaseQueued
	mt.mu.Unlock()
	m.wg.Add(1)
	go m.loop(mt)
}

// Close 关闭 Manager（F2 审查）：① 取消所有未终态任务（运行/排队经取消
// 链与标志，暂停由 finishCanceled 兜底删半成品——与逐个 Cancel 等价）；
// ② 等待全部任务 goroutine（loop）退出后才返回——runX 内部的
// worker/监视/合并线程均由 runX 自行 join，loop 退出即全链收口；
// ③ 幂等——重复调用直接返回。Close 后 Submit 报 ErrClosed，Resume/Retry
// 不再复活任务；Get 对未被淘汰的登记条目仍可用。
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return // 幂等
	}
	m.closed = true
	mts := make([]*managedTask, 0, len(m.tasks))
	for _, mt := range m.tasks {
		mts = append(mts, mt)
	}
	m.mu.Unlock()

	for _, mt := range mts {
		m.Cancel(mt.id)
	}
	m.wg.Wait() // 全部 loop 退出（Add 先于 go loop，无漏计、无漏等）
}

// isClosed Close 状态查询（Submit/Resume/Retry 入口快速失败）。
func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// evictLocked 终态淘汰（F2 审查）：登记表超过 maxTasks 时按终态时间最早
// 淘汰**已终态**条目——Cookie/URL 等上下文随条目释放（Manager 不再持有
// 引用）；存活条目（排队/运行/暂停——dest 锁或槽位仍在用）绝不淘汰，
// 全存活顶满时允许暂时超限（有终态时 finish/后续 Submit 会再触发）。
// 被淘汰后 Get 返回 ErrTaskNotFound、Retry 无效——任务持久化与复活由
// web 层负责（§6.2 持久化上下文：断点在盘（.qmeta），web 层可重新
// Submit 复活），本层不保留尸位登记。调用方须持有 m.mu。
func (m *Manager) evictLocked() {
	if len(m.tasks) <= m.maxTasks {
		return
	}
	type ent struct {
		id string
		at time.Time
	}
	var terms []ent
	for id, mt := range m.tasks {
		mt.mu.Lock()
		if mt.phase == phaseTerminal {
			terms = append(terms, ent{id, mt.termAt})
		}
		mt.mu.Unlock()
	}
	sort.Slice(terms, func(i, j int) bool { return terms[i].at.Before(terms[j].at) })
	n := min(len(m.tasks)-m.maxTasks, len(terms))
	for _, e := range terms[:n] {
		delete(m.tasks, e.id)
	}
}

// lookup 按 id 取任务登记（不存在返回 nil）。
func (m *Manager) lookup(id string) *managedTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[id]
}

// Get 查询任务进度快照（任务不存在返回 ErrTaskNotFound）。
func (m *Manager) Get(id string) (Progress, error) {
	mt := m.lookup(id)
	if mt == nil {
		return Progress{}, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}
	mt.mu.Lock()
	defer mt.mu.Unlock()
	if mt.task == nil {
		return Progress{State: "pending"}, nil
	}
	p := mt.task.progress()
	switch mt.phase {
	case phaseQueued:
		p.State = "pending"
	case phasePaused:
		p.State = "paused"
	case phaseTerminal:
		p.State = mt.terminal
	}
	return p, nil
}

// ---- managedTask 内部（mu 保护） -------------------------------------------

func (mt *managedTask) isCanceled() bool {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	return mt.canceled
}

func (mt *managedTask) isPaused() bool {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	return mt.paused
}

func (mt *managedTask) setRunning(tk *Task, url string) {
	mt.mu.Lock()
	mt.task, mt.phase = tk, phaseRunning
	mt.lr.URL = url // 换链后最新直链（诊断/后续刷新的「旧 URL」基准）
	mt.mu.Unlock()
}

func (mt *managedTask) setCancel(c context.CancelFunc) {
	mt.mu.Lock()
	mt.cancel = c
	mt.mu.Unlock()
}

func (mt *managedTask) setTerminal(state string, err error) {
	mt.mu.Lock()
	mt.phase, mt.terminal, mt.termErr = phaseTerminal, state, err
	mt.termAt = time.Now() // F2 淘汰排序键
	mt.mu.Unlock()
}

func (mt *managedTask) setPhase(p taskPhase) {
	mt.mu.Lock()
	mt.phase = p
	mt.mu.Unlock()
}

// ---- 包内辅助 ---------------------------------------------------------------

// sumW 求和（恢复对账后的进度起点 done=ΣW，§3.6）。
func sumW(w []int64) int64 {
	var t int64
	for _, v := range w {
		t += v
	}
	return t
}

// causeStatusIn 沿错误链下钻找 TaskError.Status 是否命中给定状态码集
// （与 causeIs4xx 同构；§2.10 自愈判据 401/403/412——重试耗尽的包装下
// 底层状态码仍须被认出）。
func causeStatusIn(err error, statuses ...int) bool {
	var te *TaskError
	for err != nil {
		if errors.As(err, &te) {
			for _, s := range statuses {
				if te.Status == s {
					return true
				}
			}
			err = te.Cause
			continue
		}
		err = errors.Unwrap(err)
	}
	return false
}
