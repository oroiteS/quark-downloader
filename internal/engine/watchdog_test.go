package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// 停滞看门狗（§3.9）与完整性终判（§3.11）测试：
//   - 看门狗纯逻辑用注入时钟（Tick(now) 传假时刻，threshold 压成秒级语义不变）；
//   - 「读真被掐」用真 httptest 挂起连接（写 1KB 后 hold 到 r.Context().Done()）
//     + 毫秒级阈值：不真掐的话 TCP 无超时要挂几分钟，10s 预算必炸；
//   - 终判矩阵走 runA 集成（取消/残留错误注入在 done==total 之后——
//     「取消瞬间恰已完成」的竞态锚点）+ verdict 纯逻辑边界。

// ---- 看门狗纯逻辑（注入时钟） ----------------------------------------------

// 停滞 45s 触发（§3.9 逐字 now-lastAdvance ≥ 45s）：无增长到 44.999s 不触发、
// 45s 触发；触发即记 stalled 错误（firstErr 命中 ErrStalled）+ 看门狗级取消
// （linked ctx 真被 cancel——worker 阻塞的读靠它解除）；触发幂等。
func TestWatchdogTicksToStall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tk := &Task{Total: 100}
	tk.cancel = cancel
	w := newWatchdog(tk, 45*time.Second)

	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	w.Tick(base) // 基线锚定（零值 lastAdvance 落在首拍）
	w.Tick(base.Add(44*time.Second + 999*time.Millisecond))
	if w.Stalled() {
		t.Fatal("44.999s 无增长不应触发停滞")
	}
	if ctx.Err() != nil {
		t.Fatal("未触发不应取消")
	}
	w.Tick(base.Add(45 * time.Second))
	if !w.Stalled() {
		t.Fatal("45s 无增长应触发停滞")
	}
	if ctx.Err() == nil {
		t.Fatal("触发必须级联取消（真掐 worker 阻塞读的 linked cancellation）")
	}
	tk.errMu.Lock()
	first := tk.firstErr
	tk.errMu.Unlock()
	if first == nil || !errors.Is(first, ErrStalled) {
		t.Fatalf("firstErr = %v, want ErrStalled 包装", first)
	}
	// 幂等：触发后继续 Tick 不重复 fail/cancel（firstErr 保持首个）。
	w.Tick(base.Add(90 * time.Second))
	tk.errMu.Lock()
	first2 := tk.firstErr
	tk.errMu.Unlock()
	if first2 != first {
		t.Fatalf("触发应幂等, firstErr 被覆盖: %v -> %v", first, first2)
	}
}

// done 增长刷新 lastAdvance（§3.9 逐字）：中途有前进则 45s 窗口从新时刻起算。
func TestWatchdogDoneAdvanceRefreshes(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	tk := &Task{Total: 100}
	tk.cancel = cancel
	w := newWatchdog(tk, 45*time.Second)

	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	w.Tick(base)
	tk.done.Store(10)
	w.Tick(base.Add(20 * time.Second)) // done 增长 → 刷新 lastAdvance
	w.Tick(base.Add(64 * time.Second)) // 距上次前进 44s < 45s → 不触发
	if w.Stalled() {
		t.Fatal("前进后 44s 不应触发停滞")
	}
	w.Tick(base.Add(66 * time.Second)) // 距上次前进 46s → 触发
	if !w.Stalled() {
		t.Fatal("前进后 46s 应触发停滞")
	}
}

// 用户暂停中刷新 lastAdvance（§3.9 逐字：暂停是意愿不是故障）：暂停期间
// done 不增长也绝不判停滞；解除后从解除时刻重新起算 45s。
func TestWatchdogPauseRefreshes(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	tk := &Task{Total: 100}
	tk.cancel = cancel
	w := newWatchdog(tk, 45*time.Second)

	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	w.SetPaused(true)
	w.Tick(base)
	w.Tick(base.Add(100 * time.Second)) // 暂停中远超 45s：不触发
	if w.Stalled() {
		t.Fatal("用户暂停中不应判停滞（暂停是意愿不是故障）")
	}
	w.SetPaused(false)
	w.Tick(base.Add(144*time.Second + 500*time.Millisecond)) // 解除后 44.5s：不触发
	if w.Stalled() {
		t.Fatal("解除暂停后 44.5s 不应触发")
	}
	w.Tick(base.Add(146 * time.Second)) // 解除后 46s：触发
	if !w.Stalled() {
		t.Fatal("解除暂停后 46s 应触发停滞")
	}
}

// 收尾合并阶段不做停滞判定（§3.9 逐字：worker 全结束、只剩磁盘 I/O）：
// EnterMerge 后停滞判定关闭、Merging 状态位上桌（进度改报「正在合并分片」
// 的载荷位；合并吞吐是 Task 12 的活）。
func TestWatchdogMergeSkips(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	tk := &Task{Total: 100}
	tk.speed = NewSpeedometer() // progress() 的速度计依赖
	tk.cancel = cancel
	w := newWatchdog(tk, 45*time.Second)
	tk.watchdog = w // 挂上以验证 progress() 状态位接线

	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	w.EnterMerge()
	w.Tick(base)
	w.Tick(base.Add(999 * time.Second)) // 合并阶段永不判停滞
	if w.Stalled() {
		t.Fatal("合并阶段不应判停滞")
	}
	if !w.Merging() {
		t.Fatal("EnterMerge 后 Merging 应为 true")
	}
	if p := tk.progress(); !p.Merging {
		t.Fatal("progress() 应带 Merging 状态位（§3.9 进度改报合并中）")
	}
}

// ---- 完整性终判（§3.11） ----------------------------------------------------

// verdict 纯逻辑矩阵：cancelled 或 errors 非空时 done ≥ total（模式 A）
// → 按成功处理（Note 注明传输错误但数据完整）；done < total → 维持失败。
// verdict 不猜错误来源——错误计数只是过程指标，数据是硬事实。
func TestVerdictMatrix(t *testing.T) {
	tk := &Task{Total: 100}
	tk.done.Store(99)
	if v := tk.verdict(); v.Success {
		t.Fatalf("done=99 < total=100 应判失败, got %+v", v)
	}
	tk.done.Store(100)
	v := tk.verdict()
	if !v.Success {
		t.Fatalf("done=100 = total=100 应按成功处理, got %+v", v)
	}
	if v.Note == "" {
		t.Fatal("成功注记不应为空（日志注明有传输错误但数据完整）")
	}
}

// runA 接线：取消瞬间任务恰已完成（done==total 后外部 cancel）→ 完整性
// 终判先行，按成功处理——绝不删完整文件（Task 8 审查锚点：终判必须插在
// finalizeCleanup 的删除动作之前）。
func TestRunACancelAtCompleteMoment(t *testing.T) {
	total := int64(2 << 20) // 4 片
	data := testPattern(total)
	srv, _ := newRangeServer(t, data)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk := newFastTask(t, dest, srv.URL, 1, total)
	parent, pcancel := context.WithCancel(context.Background())

	// done==total 的瞬间取消（此时所有字节已落盘，仅剩 worker 收尾退出）。
	go func() {
		for tk.done.Load() < total {
		}
		pcancel()
	}()

	if err := tk.runA(parent); err != nil {
		t.Fatalf("取消瞬间恰已完成应按成功处理, got %v", err)
	}
	if p := tk.progress(); p.State != "done" {
		t.Errorf("终态 = %q, want done", p.State)
	}
	got, rerr := os.ReadFile(dest)
	if rerr != nil {
		t.Fatalf("完整文件被删（终判先于删除的锚点被破）: %v", rerr)
	}
	if !bytes.Equal(got, data) {
		t.Error("文件内容应完整")
	}
	m, err := LoadQMeta(tk.qmetaPath())
	if err != nil {
		t.Fatalf("完成凭证 .qmeta 应保留: %v", err)
	}
	for i, w := range m.W {
		if w != tk.Segs[i].End-tk.Segs[i].Start+1 {
			t.Fatalf("片 %d W=%d 未全满", i, w)
		}
	}
}

// runA 接线：errors 非空但数据完整（§3.11 依据原文的 987MB 事故——错误
// 计数只是过程指标，某片失败记过 error 但数据后来补齐）→ 按成功处理。
// 构造：全量正常下载完成后注入残留 firstErr（fail 的级联取消此刻只影响
// 已排干的收尾），终判翻转终态为 done、文件保留。
func TestRunAFailAfterCompleteKeepsFile(t *testing.T) {
	total := int64(2 << 20) // 4 片
	data := testPattern(total)
	srv, _ := newRangeServer(t, data)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk := newFastTask(t, dest, srv.URL, 1, total)

	// done==total 后注入残留错误（模拟「补齐后残留 error」的过程指标）。
	go func() {
		for tk.done.Load() < total {
		}
		tk.fail(&TaskError{Kind: KindRetryExhausted, Detail: "残留错误：该片后来被补齐"})
	}()

	if err := tk.runA(context.Background()); err != nil {
		t.Fatalf("errors 非空但数据完整应按成功处理, got %v", err)
	}
	if p := tk.progress(); p.State != "done" {
		t.Errorf("终态 = %q, want done", p.State)
	}
	got, rerr := os.ReadFile(dest)
	if rerr != nil || !bytes.Equal(got, data) {
		t.Errorf("完整文件应保留且内容一致 (%v)", rerr)
	}
}

// ---- F1（审查）：注记出口 ------------------------------------------------------

// captureRecords 捕获 slog 记录的极简 handler（stdlib Handler 接口四方法）。
type captureRecords struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *captureRecords) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureRecords) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.recs = append(h.recs, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h *captureRecords) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureRecords) WithGroup(string) slog.Handler      { return h }

func (h *captureRecords) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.recs)
}

// 翻转分支的注记必须落日志（F1）：Verdict.Note 若只生成即丢，生产中
// 「有传输错误但数据完整」与正常成功不可区分。
//
// 测试方式选 **slog.SetDefault 注入捕获 handler** 而非对 Note 生成路径
// 断言：① 后者（TestVerdictMatrix 的 v.Note != ""）只证明字符串存在，
// 不证明它被产出——恰是 F1 指出的缺口；② 不给 Task 新增注入字段，符合
// 「F1 只动注记出口」的修复口径；③ 生产出口正是默认 logger，SetDefault
// 断言的是真实出口本身。包内测试无 t.Parallel（串行），defer 还原后无
// 全局状态泄漏。
func TestRunAVerdictFlipLogsNote(t *testing.T) {
	total := int64(2 << 20) // 4 片
	data := testPattern(total)
	srv, _ := newRangeServer(t, data)
	dest := filepath.Join(t.TempDir(), "file.bin")

	h := &captureRecords{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	tk := newFastTask(t, dest, srv.URL, 1, total)
	go func() {
		for tk.done.Load() < total {
		}
		tk.fail(&TaskError{Kind: KindRetryExhausted, Detail: "残留错误：该片后来被补齐"})
	}()

	if err := tk.runA(context.Background()); err != nil {
		t.Fatalf("errors 非空但数据完整应按成功处理, got %v", err)
	}
	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("翻转分支应恰落一行注记, got %d 条", len(recs))
	}
	attrs := map[string]string{}
	recs[0].Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	if attrs["task"] != tk.ID {
		t.Errorf("task = %q, want %q（TaskID）", attrs["task"], tk.ID)
	}
	if attrs["dest"] != dest {
		t.Errorf("dest = %q, want %q（文件名）", attrs["dest"], dest)
	}
	if !strings.Contains(attrs["note"], "数据完整") {
		t.Errorf("note = %q, 应含 Note 内容（有传输错误但数据完整）", attrs["note"])
	}
}

// ---- 停滞集成：挂起连接 + 短预算 ---------------------------------------------

// 停滞触发且读真被掐（§3.9 核心语义）：服务器写 1KB 后挂住不回——
// worker 阻塞在 body 读、done 停止增长；毫秒级阈值触发看门狗级取消
// （linked cancellation 真掐在飞请求，只设标志位的话 TCP 无超时要挂
// 几分钟、界面假死）。中止后断点保留（环境问题可续传）：dest + .qmeta
// 都在、W 记账保留已写字节。10s 预算 = 「真掐」的硬证据。
func TestRunAStalledCancelsBlockedRead(t *testing.T) {
	total := int64(2 << 20) // 4 片（threads=1：worker 阻塞在片 0）
	var handlerWG sync.WaitGroup
	handled := make(chan struct{}, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerWG.Add(1)
		defer handlerWG.Done()
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			t.Errorf("畸形 Range 头 %q: %v", r.Header.Get("Range"), err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, total))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(make([]byte, 1024)) // 先给 1KB（done 有增长）……
		w.(http.Flusher).Flush()
		handled <- struct{}{}
		<-r.Context().Done() // ……然后挂住：客户端不掐就永远不回
	}))
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk := newFastTask(t, dest, srv.URL, 1, total)
	tk.watchdog.threshold = 150 * time.Millisecond // 45s 注入化（sampleEvery 已 1ms）

	runErr := make(chan error, 1)
	go func() { runErr <- tk.runA(context.Background()) }()

	var err error
	select {
	case err = <-runErr:
	case <-time.After(10 * time.Second):
		t.Fatal("看门狗未能掐断挂起的读（linked cancellation 未生效，worker 假死）")
	}
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("err = %v, want ErrStalled", err)
	}
	if p := tk.progress(); p.State != "error" {
		t.Errorf("终态 = %q, want error（stalled 归失败但保留断点）", p.State)
	}
	// 中止后保留断点（§3.9 逐字：环境问题不是用户意愿，可续传）。
	if fi, serr := os.Stat(dest); serr != nil || fi.Size() != total {
		t.Errorf("半成品 dest 应保留（预分配满尺寸）: %v", serr)
	}
	m, lerr := LoadQMeta(tk.qmetaPath())
	if lerr != nil {
		t.Fatalf("断点 .qmeta 应保留: %v", lerr)
	}
	if m.W[0] != 1024 {
		t.Errorf("W[0] = %d, want 1024（已写字节断点记账）", m.W[0])
	}
	if tk.done.Load() == 0 || tk.done.Load() >= total {
		t.Errorf("done = %d, want 0 < done < total（部分进度）", tk.done.Load())
	}
	// 真掐的对称证据：连接被断开后服务器 handler 全部退出（不泄漏）。
	select {
	case <-handled:
	default:
		t.Fatal("挂起请求从未到达服务器")
	}
	done := make(chan struct{})
	go func() { handlerWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("服务器 handler 未随取消退出（连接未被真掐断）")
	}
}
