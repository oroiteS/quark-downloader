// 模式 B：独立分片 + 边合并（§3.5 模式 B 逐字 + §3.6 恢复对账接线）。
//
//	分片写到 <dest>.qparts/p%06d（6 位十进制定长编号，按名排序即按片序；
//	默认根 only——设计 G13 不做自定义根/历史根记账）
//	绝不预分配目标文件（预分配 = 开局满尺寸空洞文件，播放器读到全零）
//	专用合并线程严格按片序：目标文件必须从 0 连续增长，播放器才能边下边播
//	删除分片必须等 worker 关流后置的「完成」标记（p%06d.done）——提前删，
//	worker 后续写入落进已删除文件，数据静默丢失；盘上已完整的分片（续传
//	恢复时）由 ReconcileB ③ 主动补置标记，否则 worker 跳过、合并线程等
//	标记，互相干等死锁（规格实测事故）
//	峰值占用 ≈ 1×（目标文件 + 尚未合并的分片），不是「全下完再合并」的 2×
//	磁盘预检：启动时校验剩余空间 ≥ 待下载量（不预分配，磁盘满会中途才炸）
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 合并线程常数（§3.5 模式 B 3 逐字 sleep(100ms)；收尾宽限 10 轮）。
const (
	mergePollEvery   = 100 * time.Millisecond // 轮询周期（Task.mergeEvery 注入化）
	mergeGraceRounds = 10                     // worker 全退后等完成标记的额外轮数
)

// runB 装配并运行模式 B 主循环（§3.5 模式 B）。与 runA 共用 worker 全家桶
// （segQueue/重试/抢占/gate/watchdog/verdict/finalizeCleanup）——分化的
// 只有落盘目标：fetchOnce 按 modeB 写分片文件并置完成标记。合并线程从
// 调用方装配的 mergeNext/mergeInPart（ReconcileB 的 mergedCount/
// mergedInHead）起步，恢复路径与全新路径同一循环。
// 收尾顺序：worker 全结束 → EnterMerge（§3.9 不判停滞、进度报合并中）→
// 合并线程排干（宽限 ≤ mergeGrace×mergeEvery）→ 监视线程退 → 终态判定
// （模式 B verdict：目标文件长度 == total，差一个字节都不算成功）。
func (t *Task) runB(ctx context.Context) error {
	if len(t.Segs) == 0 {
		return errors.New("feiwu: 任务无分片（total 非法或 plan 为空）")
	}
	if t.Threads < 1 {
		return fmt.Errorf("feiwu: 并发档位非法 Threads=%d（应 ≥ 1）", t.Threads)
	}
	t.modeB = true
	t.partsDir = t.Dest + ".qparts"
	t.state.Store(int32(stateRunning))
	parent := ctx
	ctx, cancel := context.WithCancel(parent)
	t.cancel = cancel
	defer cancel()

	// 磁盘预检（参数表逐字：模式 B 启动时校验剩余空间 ≥ 待下载量）。在
	// 任何磁盘副作用（建目录）之前拦——把「磁盘满会中途才炸」提前到开局。
	if err := t.precheckDisk(); err != nil {
		t.fail(err)
		t.state.Store(int32(stateError))
		return err
	}
	if err := os.MkdirAll(t.partsDir, 0o755); err != nil {
		err = fmt.Errorf("feiwu: 创建分片目录失败: %w", err)
		t.fail(err)
		t.state.Store(int32(stateError))
		return err
	}
	if t.mergeSpeed == nil { // 直接构造 Task 的防御（newTask 已装配）
		t.mergeSpeed = NewSpeedometer()
	}

	// 片队列与 worker 池：与 runA 同式（§3.5 并发模型），fetchSeg 内部按
	// modeB 分派到分片文件。
	queue := newSegQueue(len(t.Segs))
	n := max(min(t.Threads, len(t.Segs), effMax), 1)
	workersDone := make(chan struct{})
	var workerWG, monWG, mergeWG sync.WaitGroup
	for w := range n {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			t.workerA(ctx, w, n, queue)
		}()
	}
	// 专用合并线程（§3.5 模式 B 3）：与 worker 并行，不等整片下完才合并——
	// 单片可达 16MB、单连接几十 KB/s 时一整片要几分钟，等整片会让目标文件
	// 长时间 0 字节，「边下边播」形同虚设。
	var mergeErr error
	mergeWG.Add(1)
	go func() {
		defer mergeWG.Done()
		mergeErr = t.mergeLoopB(ctx, t.mergeNext, t.mergeInPart, workersDone)
	}()
	monWG.Add(1)
	go t.monitorA(ctx, &monWG) // 监视线程复用（采样/存档/看门狗；Merging 帧走 progress 分派）

	// worker 全结束：进收尾合并阶段（§3.9 EnterMerge——只剩磁盘 I/O，
	// 永不判停滞；进度改报「正在合并分片」+ 合并吞吐）。
	workerWG.Wait()
	close(workersDone)
	if t.watchdog != nil {
		t.watchdog.EnterMerge()
	}
	// 合并线程排干后再掐监视线程：收尾合并期间进度事件继续发（Merging 帧）。
	// 宽限有界（≤ mergeGrace×mergeEvery），不会互相等死锁。
	mergeWG.Wait()
	cancel()
	monWG.Wait()

	// 终态落盘一次 .qmeta：失败/取消 = 断点（成功终态由下方成功分支随即
	// 删除，见 removeDoneQMeta）。best-effort——元数据丢失只是丢续传不丢
	// 数据，不覆盖 firstErr。
	_ = t.saveMeta()

	t.errMu.Lock()
	err := t.firstErr
	t.errMu.Unlock()
	if err == nil {
		err = mergeErr // worker 全成而合并线程磁盘 IO 失败：目标不完整，按失败走终判
	}
	if err == nil && parent.Err() != nil {
		err = parent.Err()
	}

	// 完整性终判（§3.11）+ 收尾清理：与 runA 同构；verdict 的模式 B 判据
	// 是目标文件长度 == total（dataComplete 复判）。
	if err != nil {
		if v := t.verdict(); v.Success {
			slog.Warn("feiwu: 完整性终判翻转（有传输错误但数据完整，按成功处理）",
				"task", t.ID, "dest", t.Dest, "note", v.Note)
			err = nil
		} else {
			err = t.finalizeCleanup(parent, err)
		}
	}

	switch {
	case err == nil:
		// 全部片已合并删除：连空分片目录一并清掉（best-effort；残留无害——
		// 下次对账 ④ 会扫，但「分片目录清空」是成功交付的验收面）。
		_ = os.Remove(t.partsDir)
		// 成功终态同构删除 .qmeta（与 runA 成功分支同一 removeDoneQMeta：
		// 成功即删，目录只留完整交付文件；失败/取消保留断点供续传）。
		// best-effort：删除失败仅日志，不掩盖成功（残留只是已完成任务的
		// 冗余记录，交引擎对账仍秒完成，无害）。
		t.removeDoneQMeta()
		t.state.Store(int32(stateDone))
	case isCancellation(err):
		t.state.Store(int32(stateCanceled))
	default:
		t.state.Store(int32(stateError))
	}
	return err
}

// precheckDisk 磁盘预检（参数表：剩余空间 ≥ 待下载量）。查询失败放行——
// 预检是把「磁盘满中途才炸」提前的优化，不是准入闸门；注入化（freeSpace）
// 便于测试。
func (t *Task) precheckDisk() error {
	if t.freeSpace == nil {
		return nil
	}
	todo := t.Total - t.done.Load()
	if todo <= 0 {
		return nil
	}
	free, err := t.freeSpace(filepath.Dir(t.Dest)) // 父目录（Dest 可能尚不存在，statfs 落空会让预检失效）
	if err != nil {
		return nil
	}
	if free < uint64(todo) {
		return fmt.Errorf("feiwu: 磁盘剩余空间不足（剩 %d 字节 < 待下载 %d 字节）", free, todo)
	}
	return nil
}

// mergeLoopB 专用合并线程（§3.5 模式 B 3 逐字）：
//
//	next = 第一个未合并片; inPart = 该片已合并的字节数
//	loop:
//	    onDisk = 分片文件 next 当前在盘上的长度   # worker 顺序追加写，
//	                                          # 盘上长度永远是合法前缀
//	    把 [inPart, onDisk) 段读出 append 到目标文件; 立即 flush
//	    inPart += 本次复制量
//	    若 inPart ≥ 片大小: 等 worker 置「完成」标记 → 删除分片文件 → next++
//	    若全部合并完: 退出
//	    否则 sleep(100ms)
//
// 退出路径：全部合并完（nil）；ctx 取消/失败（nil——保留现状，终态归
// runB，恢复对账从目标文件长度反推无损续起）；worker 全退而片未写满
// （nil——不会再有数据）；磁盘 IO 错误（error）。
// 收尾宽限：worker 全退（workersDone）后若该片数据已完整搬到目标却迟迟
// 未见完成标记，最多再等 mergeGrace 轮（100ms×10）即视同完成推进——
// worker 全退后不会再有任何写入，此时标记缺失属病态残留（历史版本
// 「互相干等死锁」的形态之一），数据已在目标文件里，删除分片安全。
func (t *Task) mergeLoopB(ctx context.Context, next int, inPart int64, workersDone <-chan struct{}) error {
	if next >= len(t.Segs) {
		return nil
	}
	// 只追加（§3.5 3）：目标文件从 0 连续增长。恢复时 ② 已把混入来路不明
	// 字节的尾部截回片边界，O_APPEND 起点即合法前缀末尾；全新开始（无
	// 有效 .qmeta）的旧 dest 残留由 Manager 起跑前清除（C1 最终审查）。
	dest, err := os.OpenFile(t.Dest, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("feiwu: 合并线程打开目标文件失败: %w", err)
	}
	defer dest.Close()

	every := t.mergeEvery
	if every <= 0 {
		every = mergePollEvery
	}
	graceInit := t.mergeGrace
	if graceInit <= 0 {
		graceInit = mergeGraceRounds
	}
	grace := graceInit
	workersExited := false

	for next < len(t.Segs) {
		if ctx.Err() != nil {
			// 取消/失败：保留现状退出（已搬字节都在目标文件里，恢复对账
			// ①从长度反推无损续起）；不返回错误——终态归 runB 统一判。
			return nil
		}
		onDisk, err := partOnDisk(t.partsDir, next)
		if err != nil {
			return err
		}
		if onDisk > inPart {
			n, err := copyPartRange(dest, t.partsDir, next, inPart, onDisk)
			if err != nil {
				return err
			}
			if err := dest.Sync(); err != nil { // 立即 flush（逐字）：播放器随时来读
				return fmt.Errorf("feiwu: 合并刷盘失败: %w", err)
			}
			inPart += n
			t.mergeDone.Add(n)
		}
		if t.mergeSpeed != nil {
			t.mergeSpeed.Sample(t.mergeDone.Load(), time.Now()) // 合并吞吐（Merging 帧）
		}

		size := t.Segs[next].End - t.Segs[next].Start + 1
		if inPart >= size {
			if partDoneMarked(t.partsDir, next) {
				// 完成标记已置（worker 关流后）→ 数据不会再写，删除分片安全。
				t.removePart(next)
				next++
				inPart = 0
				grace = graceInit
				continue
			}
			if workersExited {
				if grace--; grace <= 0 {
					// 收尾宽限耗尽：数据已完整搬到目标，标记缺失视同完成推进。
					t.removePart(next)
					next++
					inPart = 0
					grace = graceInit
					continue
				}
			}
		} else if workersExited {
			// worker 全退而片未写满：不会再有数据（失败/取消），使命结束。
			return nil
		}
		// 若全部合并完: 退出（循环条件）；否则 sleep(100ms)。
		select {
		case <-ctx.Done():
		case <-workersDone:
			workersExited = true
		case <-time.After(every):
		}
	}
	return nil
}

// ---- 分片目录布局（§3.6）与完成标记 -----------------------------------------

// partPath 分片文件路径：<partsDir>/p%06d（§3.6 布局逐字：6 位十进制定
// 长编号，按名排序即按片序）。
func partPath(partsDir string, idx int) string {
	return fmt.Sprintf("%s%cp%06d", partsDir, os.PathSeparator, idx)
}

// partDonePath 片完成标记路径：分片文件旁 p%06d.done。语义（§3.5 5 逐字）：
// worker **关闭文件流之后**置（关闭即 flush，「记账 = 盘上」）——合并线程
// 只认标记才删分片；提前删 = worker 后续写入落进已删除文件，数据静默丢失。
func partDonePath(partsDir string, idx int) string {
	return partPath(partsDir, idx) + ".done"
}

// partOnDisk 分片文件当前盘上长度（不存在 = 0；worker 顺序追加写，盘上
// 长度永远是合法前缀）。
func partOnDisk(partsDir string, idx int) (int64, error) {
	fi, err := os.Stat(partPath(partsDir, idx))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("feiwu: stat 分片 %d 失败: %w", idx, err)
	}
	return fi.Size(), nil
}

// partDoneMarked 片完成标记是否已置。
func partDoneMarked(partsDir string, idx int) bool {
	_, err := os.Stat(partDonePath(partsDir, idx))
	return err == nil
}

// markPartDone 置片完成标记（空文件）。worker 关流后调用；ReconcileB ③
// 对盘上已完整的分片补置（防互等死锁）。
func markPartDone(partsDir string, idx int) error {
	if err := os.WriteFile(partDonePath(partsDir, idx), nil, 0o644); err != nil {
		return fmt.Errorf("feiwu: 置片 %d 完成标记失败: %w", idx, err)
	}
	return nil
}

// removePart 删除分片文件与完成标记（best-effort：数据已复制进目标文件，
// 残留无害——下次恢复对账 ④ 会清扫）。
func (t *Task) removePart(idx int) {
	_ = os.Remove(partPath(t.partsDir, idx))
	_ = os.Remove(partDonePath(t.partsDir, idx))
}

// copyPartRange 把分片 idx 的 [from, to) 段追加到 dest（§3.5 3「把
// [inPart, onDisk) 段读出 append 到目标文件」）。段边界取自 stat 时刻，
// worker 之后即使继续追加也不影响——只搬已稳定的合法前缀。
func copyPartRange(dest io.Writer, partsDir string, idx int, from, to int64) (int64, error) {
	pf, err := os.Open(partPath(partsDir, idx))
	if err != nil {
		return 0, fmt.Errorf("feiwu: 合并打开分片 %d 失败: %w", idx, err)
	}
	defer pf.Close()
	n, err := io.Copy(dest, io.NewSectionReader(pf, from, to-from))
	if err != nil {
		return n, fmt.Errorf("feiwu: 合并分片 %d 失败: %w", idx, err)
	}
	if n != to-from { // 防御：worker 只追加不缩水，段读不满即磁盘异常
		return n, fmt.Errorf("feiwu: 合并分片 %d 短读（要 %d 得 %d）", idx, to-from, n)
	}
	return n, nil
}
