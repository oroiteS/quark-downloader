// 模式 A worker 主循环（§3.5「并发模型」+ §3.10「重试与错误分类」）：
//
//	N = min(用户档位, 片数, 512)
//	冷启动错峰 sleep(ramp × w / N)；片队列 segQueue 支持抢占重入（MPMC 工作窃取）
//	每片：gate.Acquire → W≥片大小跳过 → 片级重试循环（1 首试 + ≤4 重试, BackoffSeg 退避,
//	从 S+W 断点续, §3.10）→ 独立开流 Seek(S+W) → io.CopyBuffer 流写 → 写完即关
//	收尾（§3.10/§3.11）：完整性终判先行（cancelled/errors 非空但 done ≥ total →
//	按成功处理）→ finalizeCleanup 按错误分类表处置半成品（删/留/转单线程/4xx 兜底）
//	限流制动（§3.8）：429/503 → gate.Shrink（max(4,×0.7)，非阻塞收差额票）
//	慢连接抢占（§3.7）：监视线程每 5s 两遍扫描，掐慢连接的片级令牌 →
//	  errPreempted → 片重新入队（W 保留、不计失败不退避）
//	监视线程：200ms 采样速度 + 停滞看门狗 Tick（§3.9，45s 无前进 → 看门狗级
//	  取消）+ 每 1s 全量 SaveQMeta + gate.TickRecover
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	rand "math/rand/v2"
	"net/http"
	"os"
	"sync"
	"time"

	"quark-downloader/internal/protocol"
)

// runA 装配并运行模式 A 主循环（§3.5 模式 A）。ctx 是任务级生命周期：
// 取消即全体退出（错误=ctx.Err()、终态=canceled）。W 为恢复对账后的
// 记账起点（全新任务全零），完成后 W 全满且 .qmeta 留有完成凭证。
// 收尾顺序（§3.11 先于 §3.10）：完整性终判 → finalizeCleanup——cancelled
// 或 errors 非空但数据完整（done ≥ total）按成功处理，绝不删完整文件；
// 失败/取消走 finalizeCleanup（Range 不支持转单线程、4xx 零进度两组净头
// 兜底可能在这里翻转终态）。
func (t *Task) runA(ctx context.Context) error {
	if len(t.Segs) == 0 {
		return errors.New("feiwu: 任务无分片（total 非法或 plan 为空）")
	}
	// Threads<1 是装配错误（审查 F1）：显式拒绝而非静默钳到 1——静默降级
	// 会掩盖调用方 bug（错档位下「正常」下完文件无人察觉），静默跑 0 个
	// worker 更是全零文件报成功。在任何副作用（状态迁移/预分配）之前拦。
	if t.Threads < 1 {
		return fmt.Errorf("feiwu: 并发档位非法 Threads=%d（应 ≥ 1）", t.Threads)
	}
	t.state.Store(int32(stateRunning))
	parent := ctx // 外部任务生命周期；终态判定与用户取消判定盯它
	ctx, cancel := context.WithCancel(parent)
	t.cancel = cancel
	defer cancel()

	// ④ 落盘准备（§3.5 模式 A 1）：预分配目标文件到 total——开局即
	// 满尺寸稀疏文件，worker 直接按片偏移写。⚠️ 长度从此不再是进度
	// （§3.6 ②），进度只认 W/done。
	if err := t.preallocate(); err != nil {
		t.fail(err)
		t.state.Store(int32(stateError))
		return err
	}

	// ⑤ 片队列：抢占可重入（§3.10 抢占重入队）。closed channel 做不到
	// 「取尽后再投」，故用锁+条件变量的 segQueue：pending 空但仍有
	// worker 持片（可能被 Task 10 掐了重入）时等待，两者皆空才排干。
	// 快 worker 干完立刻取下一片，慢连接只拖自己那一小片（§3.3）。
	queue := newSegQueue(len(t.Segs))

	// N = min(用户档位, 片数, 512)（§3.5）。与 newTask 同式 max(...,1) 下钳
	// （审查 F1）：两处算法统一成结构不变量——即使入口守卫日后被重构掉，
	// N 也不可为 0。
	n := max(min(t.Threads, len(t.Segs), effMax), 1)
	var workerWG, monWG sync.WaitGroup
	for w := range n {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			t.workerA(ctx, w, n, queue)
		}()
	}
	monWG.Add(1)
	go t.monitorA(ctx, &monWG) // 监视线程（采样 + 周期落盘）

	// ⑥ 全部 worker 结束（§3.1）。此刻主动 cancel 掐掉监视线程——它的
	// 存档职责到此为止，终态 .qmeta 由下方收尾统一落盘（否则 monitorA
	// 等 ctx、defer cancel 又排在 wg.Wait 之后，互相等死锁）。
	workerWG.Wait()
	cancel()
	monWG.Wait()

	// 终态落盘一次 .qmeta：成功 = W 全满的完成凭证；失败/取消 = 断点。
	// best-effort——元数据丢失只是丢续传不丢数据，不覆盖 firstErr。
	_ = t.saveMeta()

	t.errMu.Lock()
	err := t.firstErr
	t.errMu.Unlock()
	if err == nil && parent.Err() != nil {
		err = parent.Err() // 外部取消（内部失败已走 firstErr；自己收尾的 cancel 不算）
	}

	// ⑦ 完整性终判（§3.11，先于 finalizeCleanup 的删除动作——T8 审查
	//    锚点）：cancelled 或 errors 非空时，报失败之前先验证数据本身——
	//    done ≥ total（模式 A）→ 按成功处理（Note 注明有传输错误但数据
	//    完整）。绝不「取消瞬间任务恰已完成」还删完整文件。模式 B 的
	//    文件长度复判由 Task 12 在 verdict 内扩展。
	if err != nil {
		if v := t.verdict(); v.Success {
			// F1（审查）：Note 出口——翻转分支必须落一行 slog 注记，否则
			// 生产中「有传输错误但数据完整」与正常成功不可区分。stdlib
			// 默认 logger，零新增依赖；测试经 slog.SetDefault 注入捕获
			// handler 断言产出（TestRunAVerdictFlipLogsNote）。
			slog.Warn("feiwu: 完整性终判翻转（有传输错误但数据完整，按成功处理）",
				"task", t.ID, "dest", t.Dest, "note", v.Note)
			err = nil // 数据完整：翻转终态为 done，跳过清理删除
		} else {
			// §3.10 收尾：错误分类 → 半成品生死（删/留/转单线程/4xx 兜底）。
			// 兜底成功会翻转终态为 done；兜底途中用户取消按取消归类。
			err = t.finalizeCleanup(parent, err)
		}
	}

	switch {
	case err == nil:
		t.state.Store(int32(stateDone))
	case isCancellation(err):
		t.state.Store(int32(stateCanceled))
	default:
		t.state.Store(int32(stateError))
	}
	return err
}

// preallocate 模式 A 预分配（§3.5 模式 A 1「SetLength 到 total」）：
// os.Truncate 语义（不存在先创建；短文件扩到 total、超长截回——超长
// 残留按 §3.6 ① 本就该在 ReconcileA 截过，这里兜底）。
func (t *Task) preallocate() error {
	f, err := os.OpenFile(t.Dest, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("feiwu: 预分配打开目标文件失败: %w", err)
	}
	defer f.Close()
	if err := f.Truncate(t.Total); err != nil {
		return fmt.Errorf("feiwu: 预分配 %d 字节失败: %w", t.Total, err)
	}
	return nil
}

// segQueue 片队列：MPMC 工作窃取 + 抢占重入（§3.5/§3.10）。
type segQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	pending []int // 待取片
	active  int   // 正被 worker 持有的片数
}

func newSegQueue(n int) *segQueue {
	q := &segQueue{pending: make([]int, n)}
	for i := range q.pending {
		q.pending[i] = i
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// pop 取一片：有则弹（FIFO，快 worker 窃慢片的公平性由队列唯一分发
// 保证）；pending 空但 active>0（有片在飞，可能被抢占重入）则等待；
// 两者皆空宣告排干，worker 退出。
func (q *segQueue) pop() (int, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if len(q.pending) > 0 {
			idx := q.pending[0]
			q.pending = q.pending[1:]
			q.active++
			return idx, true
		}
		if q.active == 0 {
			return 0, false
		}
		q.cond.Wait()
	}
}

// release 片处理完毕（成功/失败），归还持有计数。
func (q *segQueue) release() {
	q.mu.Lock()
	q.active--
	q.mu.Unlock()
	q.cond.Broadcast()
}

// requeue 抢占重入（§3.10）：所有权从 worker 直接转回队列，不计失败。
func (q *segQueue) requeue(idx int) {
	q.mu.Lock()
	q.pending = append(q.pending, idx)
	q.active--
	q.mu.Unlock()
	q.cond.Broadcast()
}

// workerA 是单个 worker 的主循环（§3.5 伪代码 + §3.10 重试/抢占消化）。
func (t *Task) workerA(ctx context.Context, w, n int, queue *segQueue) {
	// 冷启动错峰（§3.5）：几百路同时发起 = 同时几百次 TLS 握手，拖慢
	// 首字节且易触发风控。w=0 睡 0 立即起步；ctx 取消随时可醒。
	if d := t.ramp * time.Duration(w) / time.Duration(n); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return
		}
	}
	if t.onWorkerStart != nil {
		t.onWorkerStart(w)
	}
	buf := make([]byte, BufSize(n)) // 读缓冲 ~64MB/N，总内存钉住（§3.3）
	// 随机源（§3.10 逐字）：每 worker 独立实例——共享一个实例会被并发
	// 调用互踩状态，「抖动」退化成同步退避，恰好促成重试风暴。
	rng := rand.New(rand.NewPCG(uint64(w)+1, uint64(time.Now().UnixNano())))
	for {
		idx, ok := queue.pop()
		if !ok {
			return
		}
		// 采样登记（§3.7 供抢占看门狗）：领片即记录年龄锚点；片处理完
		//（成功/失败/被抢皆然）释放。nil 防御：直接构造 Task 的路径。
		if t.preempt != nil {
			t.preempt.begin(w, idx, time.Now())
		}
		err := t.fetchSeg(ctx, idx, buf, rng)
		if t.preempt != nil {
			t.preempt.end(w)
		}
		switch {
		case err == nil:
			queue.release()
		case errors.Is(err, errPreempted):
			// 先判抢占（§3.10/Task 10 框架）：片重新入队、不计失败、
			// 不退避、不留错误标记（fetchSeg 返回即弃暂态错误）。
			queue.requeue(idx)
		default:
			queue.release()
			t.fail(err) // 致命错误：本 worker 退出，级联取消已由 fail 发出
			return
		}
	}
}

// fetchSeg 下载单片（§3.10 片级重试）：闸门 → 续传跳过 → 重试循环
// （首试 + ≤4 次重试共 ≤5 次尝试（Ruling E3），尝试间 BackoffSeg 退避，
// 每次从 S+W 断点续——不重复下载）。
// 片级取消令牌（§3.5）独立登记，生命周期 = 整片（含退避等待）：Task 10
// 抢占看门狗 cancelSeg(idx) 只掐本片、不污染全局。「先判抢占再判取消」
// 的顺序判定框架在这里（Task 10 复用）：抢占 → errPreempted 交 workerA
// 重入队；取消 → 按取消归类（runA 归 canceled 终态）。
// W[idx] 只被持有本片的 worker 读写（队列唯一分发），记账增量的锁只为
// 监视线程的快照读（wmu）。
func (t *Task) fetchSeg(ctx context.Context, idx int, buf []byte, rng *rand.Rand) error {
	s := t.Segs[idx]
	size := s.End - s.Start + 1

	// Ruling E1：领片后先过限流闸门（§3.8；Task 9 实装，默认直通）。
	if err := t.gate.Acquire(ctx); err != nil {
		return err
	}
	defer t.gate.Release()

	// 续传：W ≥ 片大小 → 跳过（§3.5 伪代码，恢复时常见）。
	if t.W[idx] >= size {
		return nil
	}

	// 片级令牌（§3.5）：抢占只掐当前片，不污染全局。懒初始化注册表
	//（直接构造 Task 的测试路径无 newTask）。
	sctx, cancelSegCtx := context.WithCancel(ctx)
	t.segMu.Lock()
	if t.segCancel == nil {
		t.segCancel = make(map[int]context.CancelFunc)
	}
	t.segCancel[idx] = cancelSegCtx
	t.segMu.Unlock()
	defer func() {
		t.segMu.Lock()
		delete(t.segCancel, idx)
		t.segMu.Unlock()
		cancelSegCtx()
	}()

	// 退避等待注入点（nil 防御：直接构造 Task 的路径无 newTask 默认值）。
	sleepFn := t.sleep
	if sleepFn == nil {
		sleepFn = sleepCtx
	}

	for attempt := 0; ; attempt++ {
		err := t.fetchOnce(sctx, idx, buf)
		if err == nil {
			return nil
		}
		// ⓪ 限流制动（§3.8 逐字，Task 9 接线）：收到 429/503 →
		// gate.Shrink()（gateLimit=max(4,×0.7)，非阻塞收差额票）。
		// 放在抢占/取消判定之前——限流是全局急务；429/503 错误链不含
		// 抢占/取消哨兵（statusOf=0），互不误触。Shrink 内部绝不阻塞，
		// 不会拖住本 worker。
		if isRateLimited(statusOf(err)) {
			t.gate.Shrink()
		}
		// ① 先判抢占（Task 10 顺序判定框架）：片级令牌已取消而全局
		//    未取消 → 抢占，交 workerA 重入队（不计失败、不退避）。
		if sctx.Err() != nil && ctx.Err() == nil {
			return errPreempted
		}
		// ② 再判取消：全局取消按取消归类（审查 F2 的分类点统一在此，
		//    「干净 EOF + 取消」竞态不再误判短体致命——短体已可重试，
		//    取消则根本不该重试）。
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if !retryableSeg(err) {
			return err // 200/403 等致命类：重试无意义
		}
		// attempt 从 0 起计失败次数：首试失败后已重试 attempt 次；仅当
		// 首试 + segRetryMax 次重试（第 5 次尝试）仍失败才耗尽（Ruling E3）。
		if attempt >= segRetryMax {
			// §3.10「片重试耗尽」：入 errors、任务失败（终判归 Task 11）。
			// 保留底层 Status——416 等可重试类耗尽后仍是 4xx，兜底判据
			//（causeIs4xx）要能认出。
			return &TaskError{
				Kind:   KindRetryExhausted,
				Status: statusOf(err),
				Cause:  err,
				Detail: fmt.Sprintf("片 %d 尝试 %d 次耗尽", idx, segRetryMax+1),
			}
		}
		// 退避等待（§3.10 逐字：重试等待期间可被抢占/取消）：监听片级
		// 令牌与任务 ctx，先判抢占再判取消（同上）。
		if werr := sleepFn(sctx, BackoffSeg(attempt, rng.Float64())); werr != nil {
			if sctx.Err() != nil && ctx.Err() == nil {
				return errPreempted
			}
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			return werr
		}
	}
}

// fetchOnce 单次片尝试（fetchSeg 重试循环的循环体）：Range GET → 独立
// 开流 Seek(S+W) → 写 → 写完即关。错误分类（Task 7 交接的非 206 分化）：
// 200 = 服务器无视 Range，分片写会从片偏移写坏整个文件——一个字节都
// 不读；408/416/429/5xx → 片级可重试；其余 4xx = 确定性拒绝（不重试，
// 失败后走任务级 4xx 兜底判据）。短体（§3.10 重审：连接中断的可恢复
// 形态，W 已记账、断点续不重复下载）同样返回可重试错误。
func (t *Task) fetchOnce(sctx context.Context, idx int, buf []byte) error {
	s := t.Segs[idx]
	w := t.W[idx]
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, t.URL, nil)
	if err != nil {
		return err
	}
	req.Header = DownloadHeaders(t.CookieStr)                             // 全套下载头（§3.4）
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", s.Start+w, s.End)) // end 闭区间

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		if resp.StatusCode == http.StatusOK {
			return &TaskError{
				Kind:   KindRangeUnsupported,
				Status: http.StatusOK,
				Cause:  ErrRangeUnsupported,
				Detail: fmt.Sprintf("片 %d: 服务器无视 Range 回 200", idx),
			}
		}
		return &TaskError{
			Kind:   httpKind(resp.StatusCode),
			Status: resp.StatusCode,
			Detail: fmt.Sprintf("片 %d: 状态 %d", idx, resp.StatusCode),
		}
	}

	// 每片独立开流 → Seek(S+W) → 写 → 写完即关（§3.5：关闭即 flush，
	// 「记账 = 盘上」，不给续传留「缓冲未落盘」陷阱；省几次 open 不值）。
	f, err := os.OpenFile(t.Dest, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("feiwu: 打开目标文件写片 %d 失败: %w", idx, err)
	}
	defer f.Close()
	if _, err := f.Seek(s.Start+w, io.SeekStart); err != nil {
		return fmt.Errorf("feiwu: Seek 到 %d 失败: %w", s.Start+w, err)
	}

	remain := s.End - s.Start + 1 - w
	cw := &segWriter{t: t, idx: idx, f: f} // 边写边记账（W/done 逐块递增）
	written, err := io.CopyBuffer(cw, io.LimitReader(resp.Body, remain), buf)
	if err != nil {
		return fmt.Errorf("片 %d 写入失败: %w", idx, err)
	}
	if written < remain { // LimitReader 防越界：多余字节绝不越过片尾
		// 取消/抢占的归类统一交给 fetchSeg 的顺序判定（①②），这里只报
		// 短体事实（已可重试，从 S+W 断点续）。
		return fmt.Errorf("片 %d: %w（要 %d 得 %d）", idx, errShortBody, remain, written)
	}
	return nil
}

// cancelSeg 掐断片 idx 的片级取消令牌（§3.5/§3.7）：只取消该片在飞
// 请求与退避等待，不污染全局取消。Task 10 抢占看门狗的入口；返回是否
// 确有在飞令牌。
func (t *Task) cancelSeg(idx int) bool {
	t.segMu.Lock()
	defer t.segMu.Unlock()
	if c := t.segCancel[idx]; c != nil {
		c()
		return true
	}
	return false
}

// fallbackHeaders 直链 4xx 兜底的两组净头（§3.10 逐字：仅 UA+Referer+
// Origin → 仅 UA）。比全套下载头（§3.4）干净：不带 Cookie/Accept/
// Sec-Fetch 等字段——4xx 失败常因某些头触发风控差异，逐组收窄定位。
func fallbackHeaders() []http.Header {
	h1 := http.Header{}
	h1.Set("User-Agent", protocol.UserAgent)
	h1.Set("Referer", "https://pan.quark.cn/")
	h1.Set("Origin", "https://pan.quark.cn")
	h2 := http.Header{}
	h2.Set("User-Agent", protocol.UserAgent)
	return []http.Header{h1, h2}
}

// runSingle 单线程全量下载（§3.10：Range 不支持转单线程 / 4xx 零进度
// 兜底共用）：不带 Range、期望 200，从 0 写满 total（O_TRUNC 重写）；
// 成功后 W 全满并落 .qmeta 完成凭证（§3.6）。
func (t *Task) runSingle(ctx context.Context, headers http.Header) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		return err
	}
	req.Header = headers
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &TaskError{
			Kind:   httpKind(resp.StatusCode),
			Status: resp.StatusCode,
			Detail: fmt.Sprintf("单线程全量得到 %d", resp.StatusCode),
		}
	}
	f, err := os.OpenFile(t.Dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("feiwu: 单线程打开目标文件失败: %w", err)
	}
	defer f.Close()
	written, err := io.Copy(f, resp.Body)
	if err != nil {
		return fmt.Errorf("feiwu: 单线程写入失败: %w", err)
	}
	if written != t.Total {
		return &TaskError{Kind: KindFatal, Detail: fmt.Sprintf("单线程全量短体（要 %d 得 %d）", t.Total, written)}
	}
	t.wmu.Lock()
	for i, s := range t.Segs {
		t.W[i] = s.End - s.Start + 1
	}
	t.wmu.Unlock()
	t.done.Store(t.Total)
	return t.saveMeta() // 完成凭证（best-effort 由调用方口径覆盖）
}

// removePartial 删除半成品（§3.10 表「删除」动作）：dest + .qmeta
// （模式 B 的分片目录由 Task 12 并入同一入口）。best-effort：清理失败
// 不掩盖任务主错误（返回给调用方决定是否忽略）。
func (t *Task) removePartial() error {
	err1 := os.Remove(t.Dest)
	err2 := os.Remove(t.qmetaPath())
	if err1 != nil && !os.IsNotExist(err1) {
		return err1
	}
	if err2 != nil && !os.IsNotExist(err2) {
		return err2
	}
	return nil
}

// zeroProgress 磁盘上是否毫无进度（§3.10 兜底判据「.qmeta 全零」）：
// W 任一片非零即有进度——**有进度则绝不走兜底**（兜底会清空半成品，
// 不能毁掉用户几小时的进度）。
func (t *Task) zeroProgress() bool {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	for _, w := range t.W {
		if w != 0 {
			return false
		}
	}
	return true
}

// finalizeCleanup 按 §3.10 错误分类表处置半成品并兜底（runA 终态判定
// 之后调用；返回值是最终任务错误——兜底成功为 nil，终态由调用方翻转）：
//
//	中断来源              判据                半成品处理
//	用户主动取消          外部取消令牌触发    删除 dest + .qmeta（+分片目录）
//	停滞看门狗中止        stalled 标志        保留（环境问题，可续传）
//	反压期间取消等        取消但非外部令牌    保留
//	Range 不支持          分片请求回 200      删除后转单线程全量下载
//	片重试耗尽/任务失败   errors 非空         见完整性终判（Task 11；保留）
//	直链 4xx 且磁盘零进度 .qmeta 全零         两组净头按序兜底；有进度绝不走
func (t *Task) finalizeCleanup(parent context.Context, err error) error {
	if err == nil {
		return nil
	}
	// ① stalled 先于取消判定（看门狗中止也伴随内部 cancel，但它是环境
	//    问题不是用户意志）：断点全保留。
	if errors.Is(err, ErrStalled) {
		return err
	}
	// ② 取消类：外部取消令牌触发 = 用户主动取消 → 删；其它取消（反压
	//    等，Task 9/10 引入）→ 保留。用户取消包上 ErrUserCancel 锚点，
	//    errors.Is 对 ErrUserCancel 与 context.Canceled 双命中。
	if isCancellation(err) {
		if parent.Err() != nil {
			_ = t.removePartial()
			return fmt.Errorf("%w: %w", ErrUserCancel, err)
		}
		return err
	}
	// ③ Range 不支持：删后转单线程全量（原全套头；服务器既然无视
	//    Range，断点无意义，从头整下）。
	if ClassifyError(err) == KindRangeUnsupported {
		_ = t.removePartial()
		if rerr := t.runSingle(parent, DownloadHeaders(t.CookieStr)); rerr == nil {
			return nil
		} else if parent.Err() != nil {
			_ = t.removePartial() // 兜底途中用户取消同样是用户取消：删
			return parent.Err()   // 按取消归类
		} else {
			err = rerr // 转单线程也败：用最新错误继续判 4xx 兜底
		}
	}
	// ④ 直链 4xx 兜底（§3.10 逐字）：失败原因属 4xx（沿 Cause 链下钻，
	//    含重试耗尽包装下的 416/403）且磁盘零进度 → 按序换两组净头
	//    单线程全量重试；有进度则绝不走。
	if causeIs4xx(err) && t.zeroProgress() {
		for _, h := range fallbackHeaders() {
			_ = t.removePartial()
			rerr := t.runSingle(parent, h)
			if rerr == nil {
				return nil
			}
			if parent.Err() != nil {
				_ = t.removePartial() // 同上：用户取消的删除语义
				return parent.Err()
			}
			err = rerr
		}
	}
	// ⑤ 片重试耗尽/任务失败：见完整性终判（Task 11）——本任务保留半成品。
	return err
}

// segWriter 把「写盘」与「记账」绑成一个动作（§3.5 伪代码
// `while n = read: write; W += n; done += n` 的逐块语义——监视线程才能
// 看到实时进度）。W 在 wmu 内递增供快照；done 原子累加。
type segWriter struct {
	t   *Task
	idx int
	f   *os.File
}

func (c *segWriter) Write(p []byte) (int, error) {
	n, err := c.f.Write(p)
	if n > 0 {
		c.t.wmu.Lock()
		c.t.W[c.idx] += int64(n)
		c.t.wmu.Unlock()
		c.t.done.Add(int64(n))
	}
	return n, err
}

// monitorA 是监视线程：sampleEvery 节拍喂 EWMA 速度计（§3.12）+ 发进度
// 事件；每 qmetaEvery 全量 SaveQMeta（§3.6「每 1s 全量重写」）；每
// preemptEvery 执行一轮慢连接抢占扫描（§3.7 两遍扫描，周期注入化）。
// ctx 结束即退（终态落盘由 runA 收尾统一做）。
func (t *Task) monitorA(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	tk := time.NewTicker(t.sampleEvery)
	defer tk.Stop()
	nextSave := time.Now().Add(t.qmetaEvery)
	nextScan := time.Now().Add(t.preemptEvery)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tk.C:
			t.gate.TickRecover(now) // §3.8 恢复：距上次限流 ≥30s 且 limit<N → +1（节奏判定在 Gate 内，200ms 一跳只是喂表）
			t.speed.Sample(t.done.Load(), now)
			if t.watchdog != nil {
				t.watchdog.Tick(now) // §3.9 停滞看门狗：每 200ms 读 done，45s 无前进 → stalled + 看门狗级取消
			}
			if t.onEvent != nil {
				t.onEvent(t.progress())
			}
			if !now.Before(nextSave) {
				_ = t.saveMeta()
				nextSave = now.Add(t.qmetaEvery)
			}
			if t.preempt != nil && !now.Before(nextScan) {
				t.preempt.Scan(now) // §3.7 抢占扫描（200ms 节拍内按 5s 分频）
				nextScan = now.Add(t.preemptEvery)
			}
		}
	}
}
