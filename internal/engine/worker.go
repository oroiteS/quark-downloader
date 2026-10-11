// 模式 A worker 主循环（§3.5「并发模型」模式 A 部分，逐字伪代码）：
//
//	N = min(用户档位, 片数, 512)
//	冷启动错峰 sleep(ramp × w / N)；片队列 chan int 即工作窃取（MPMC）
//	每片：gate.Acquire → W≥片大小跳过 → 片级 ctx → GET Range →
//	独立开流 Seek(S+W) → io.CopyBuffer 流写 → 写完即关（记账=盘上）
//	监视线程：200ms 采样速度 + 每 1s 全量 SaveQMeta
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

var (
	// errRangeIgnored：分片请求得到非 206（典型 200——服务器无视 Range）。
	// 分片写会从片偏移写坏整个文件，绝不能落盘；本任务记为致命错误
	// 终止任务，Task 8 接手转「单线程整下」fallback（§3.5）。
	errRangeIgnored = errors.New("feiwu: 服务器忽略 Range（分片请求得到非 206）")

	// errShortBody：206 响应体在区间末尾之前提前结束（连接半途断开等）。
	// 本任务同样记致命错误，Task 8 换成片级重试（§3.10）。
	errShortBody = errors.New("feiwu: 片响应体短于请求区间")
)

// runA 装配并运行模式 A 主循环（§3.5 模式 A）。ctx 是任务级生命周期：
// 取消即全体退出（错误=ctx.Err()、终态=canceled）。W 为恢复对账后的
// 记账起点（全新任务全零），完成后 W 全满且 .qmeta 留有完成凭证。
func (t *Task) runA(ctx context.Context) error {
	if len(t.Segs) == 0 {
		return errors.New("feiwu: 任务无分片（total 非法或 plan 为空）")
	}
	t.state.Store(int32(stateRunning))
	parent := ctx // 外部任务生命周期；终态判定盯它（派生 ctx 还被自己收尾掐）
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

	// ⑤ 片队列：chan int 缓冲 MAX_SEG，全片入队不阻塞；close 后取尽
	// 即自然退出。Go channel 即 MPMC 工作窃取——快 worker 干完立刻取
	// 下一片，慢连接只拖自己那一小片（§3.3 设计原理）。
	queue := make(chan int, segCountMax)
	for i := range t.Segs {
		queue <- i
	}
	close(queue)

	n := min(t.Threads, len(t.Segs), effMax) // N = min(用户档位, 片数, 512)
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
	switch {
	case err != nil:
		t.state.Store(int32(stateError))
	case parent.Err() != nil: // 外部取消（内部失败已走上一分支；自己收尾的 cancel 不算）
		t.state.Store(int32(stateCanceled))
		err = parent.Err()
	default:
		t.state.Store(int32(stateDone))
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

// workerA 是单个 worker 的主循环（§3.5 伪代码逐字）。
func (t *Task) workerA(ctx context.Context, w, n int, queue <-chan int) {
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
	for idx := range queue {
		if err := t.fetchSeg(ctx, idx, buf); err != nil {
			t.fail(err)
			return // 致命错误：本 worker 退出，级联取消已由 fail 发出
		}
	}
}

// fetchSeg 下载单片：闸门 → 续传跳过 → Range GET → 独立开流写完即关。
// W[idx] 只被持有本片的 worker 读写（队列唯一分发），记账增量的锁只为
// 监视线程的快照读（wmu）。
func (t *Task) fetchSeg(ctx context.Context, idx int, buf []byte) error {
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

	// 片级取消（§3.5）：抢占只掐当前片，不污染全局。本任务从任务 ctx
	// 派生即可；Task 8/10 换成独立片级令牌（引擎 client 无独立拨号
	// 超时——片级请求生命周期全由这个 ctx 控制）。
	sctx, cancelSeg := context.WithCancel(ctx)
	defer cancelSeg()

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

	// 非 206（典型 200 = 服务器忽略 Range）：body 是全文件，从片偏移
	// 写进去会写坏整个文件——一个字节都不读，记错误语义（Task 8 转
	// 单线程整下）。
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("片 %d: %w（得到 %d）", idx, errRangeIgnored, resp.StatusCode)
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

	remain := size - w
	cw := &segWriter{t: t, idx: idx, f: f} // 边写边记账（W/done 逐块递增）
	written, err := io.CopyBuffer(cw, io.LimitReader(resp.Body, remain), buf)
	if err != nil {
		return fmt.Errorf("片 %d 写入失败: %w", idx, err)
	}
	if written < remain { // LimitReader 防越界：多余字节绝不越过片尾
		return fmt.Errorf("片 %d: %w（要 %d 得 %d）", idx, errShortBody, remain, written)
	}
	return nil
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
// 事件；每 qmetaEvery 全量 SaveQMeta（§3.6「每 1s 全量重写」）。ctx 结束
// 即退（终态落盘由 runA 收尾统一做）。
func (t *Task) monitorA(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	tk := time.NewTicker(t.sampleEvery)
	defer tk.Stop()
	nextSave := time.Now().Add(t.qmetaEvery)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tk.C:
			t.speed.Sample(t.done.Load(), now)
			if t.onEvent != nil {
				t.onEvent(t.progress())
			}
			if !now.Before(nextSave) {
				_ = t.saveMeta()
				nextSave = now.Add(t.qmetaEvery)
			}
		}
	}
}
