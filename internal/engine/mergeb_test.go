package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 模式 B（§3.5 独立分片+边合并 / §3.6 恢复对账）测试：
// 分片目录 <dest>.qparts/p%06d、绝不预分配目标、合并线程严格片序、
// 片完成标记（.done）后删分片、恢复对账四步（含互等死锁场景）、
// kill 模拟写一半→恢复→比对。全部走真实 httptest Range 服务器。

// ---- 布置辅助 ---------------------------------------------------------------

// writeN 写 n 字节确定性内容到 path（对账只看长度，内容供前缀粗校验）。
func writeN(t *testing.T, path string, n int64, seed byte) {
	t.Helper()
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("布置 %s: %v", path, err)
	}
}

// statSize 断言友好版：不存在返回 -1（区别于 reconcile_test.go 的 fileSize——那边失败即 Fatal）。
func statSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return fi.Size()
}

// newFastTaskB 在 newFastTask 基础上压合并轮询周期（生产 100ms）。
func newFastTaskB(t *testing.T, dest, url string, threads int, total int64) *Task {
	t.Helper()
	tk := newFastTask(t, dest, url, threads, total)
	tk.mergeEvery = 2 * time.Millisecond
	return tk
}

// ---- 小文件全流程 -----------------------------------------------------------

// TestRunBSmall：8MB/8 线程模式 B 完成后逐字节一致、分片目录清空（空目录
// 也删）、.qmeta W 全满、进度事件 Done 单调；下载过程中目标文件始终未达
// total（绝不预分配的证据——预分配 = 开局满尺寸空洞文件）；收尾合并阶段
// 出现 Merging=true 且 Done==total 的帧（watchdog EnterMerge + 合并吞吐）。
func TestRunBSmall(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	srv, _ := newRangeServer(t, data)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk := newFastTask(t, dest, srv.URL, 8, total)
	tk.mergeEvery = 50 * time.Millisecond // 合并 16 片 ≈ 800ms > 下载时长 → 收尾必有 Merging 帧

	var evMu sync.Mutex
	var events []Progress
	sawDestLTTotal := false // 不预分配证据：某帧时刻目标文件 < total
	tk.onEvent = func(p Progress) {
		evMu.Lock()
		defer evMu.Unlock()
		events = append(events, p)
		if !sawDestLTTotal && p.Done < p.Total {
			sawDestLTTotal = statSize(dest) != total
		}
	}
	if err := tk.runB(context.Background()); err != nil {
		t.Fatalf("runB: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读回目标文件: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("目标文件与源不一致（len got=%d want=%d）", len(got), len(data))
	}
	// 分片目录清空：成功收尾连空目录一并删除（残留任何分片/标记即失败）。
	if _, err := os.Stat(dest + ".qparts"); !os.IsNotExist(err) {
		t.Errorf("分片目录应清空删除, stat err=%v", err)
	}

	m, err := LoadQMeta(dest + ".qmeta")
	if err != nil {
		t.Fatalf("完成后 .qmeta 应存在且合法: %v", err)
	}
	for i, s := range tk.Segs {
		if want := s.End - s.Start + 1; m.W[i] != want {
			t.Errorf("W[%d] = %d, want %d（完成后应全满）", i, m.W[i], want)
		}
	}

	if p := tk.progress(); p.Done != total || p.State != "done" {
		t.Errorf("progress = %+v, want Done=%d State=done", p, total)
	}

	evMu.Lock()
	defer evMu.Unlock()
	if len(events) == 0 {
		t.Fatalf("监视线程一个进度事件都没发")
	}
	for i := 1; i < len(events); i++ {
		if events[i].Done < events[i-1].Done {
			t.Errorf("进度事件 Done 回退: #%d %d < #%d %d", i, events[i].Done, i-1, events[i-1].Done)
		}
	}
	if !sawDestLTTotal {
		t.Errorf("下载过程中目标文件从未小于 total——疑似预分配（模式 B 绝不预分配）")
	}
	sawMerging := false
	for _, e := range events {
		if e.Merging {
			sawMerging = true
			if e.Done != total {
				t.Errorf("Merging 帧 Done=%d, want %d（合并阶段 worker 已全部结束）", e.Done, total)
			}
		}
	}
	if !sawMerging {
		t.Errorf("收尾合并阶段应出现 Merging=true 进度帧（正在合并分片）")
	}
}

// ---- 恢复对账四步（§3.6 逐字）----------------------------------------------

// TestReconcileBSteps 逐字覆盖 §3.6 模式 B 对账四步，segs=layout(1500,3)
// （每片 500）。记账 W 故意写错——③「W[i]=磁盘长度一律不信记账」。
func TestReconcileBSteps(t *testing.T) {
	segs := layout(1500, 3)
	if len(segs) != 3 || segs[0].End != 499 || segs[2].End != 1499 {
		t.Fatalf("布置前提: layout(1500,3) = %+v", segs)
	}

	t.Run("①目标长度反推+④已合并片", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "f")
		parts := dest + ".qparts"
		os.MkdirAll(parts, 0o755)
		writeN(t, dest, 700, 1)                            // 片0 全 + 片1 前 200
		writeN(t, filepath.Join(parts, "p000000"), 400, 2) // 已合并片的残留分片
		writeN(t, filepath.Join(parts, "p000001"), 300, 3) // 未合并，盘上 300
		m := &QMeta{Total: 1500, W: []int64{500, 0, 0}}    // 记账故意不可信

		w, mc, mih, ok := ReconcileB(dest, parts, m, segs)
		if !ok {
			t.Fatalf("ReconcileB ok=false（判据应全过）")
		}
		if mc != 1 || mih != 200 {
			t.Errorf("mergedCount=%d mergedInHead=%d, want 1/200（目标 700 = 片0 500 + 片1 200）", mc, mih)
		}
		if want := []int64{500, 300, 0}; !equalInt64s(w, want) {
			t.Errorf("w=%v, want %v（④已合并片 W=片大小；③未合并 W=磁盘长度不信记账）", w, want)
		}
		// ④：已合并片的残留分片文件删除。
		if _, err := os.Stat(filepath.Join(parts, "p000000")); !os.IsNotExist(err) {
			t.Errorf("④ 已合并片 p000000 残留应删除, stat err=%v", err)
		}
	})

	t.Run("②尾部对账截断目标", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "f")
		parts := dest + ".qparts"
		os.MkdirAll(parts, 0o755)
		writeN(t, dest, 700, 1)                            // mergedInHead=200
		writeN(t, filepath.Join(parts, "p000001"), 100, 3) // 分片 100 < 200 → 目标混入来路不明字节
		m := &QMeta{Total: 1500, W: []int64{500, 200, 0}}

		w, mc, mih, ok := ReconcileB(dest, parts, m, segs)
		if !ok {
			t.Fatalf("ok=false")
		}
		if got := statSize(dest); got != 500 {
			t.Errorf("② 目标应截断到前一该片边界 500, got %d（内容正确优先于省流量）", got)
		}
		if mc != 1 || mih != 0 {
			t.Errorf("② 后 mergedCount=%d mergedInHead=%d, want 1/0", mc, mih)
		}
		if want := []int64{500, 100, 0}; !equalInt64s(w, want) {
			t.Errorf("w=%v, want %v", w, want)
		}
	})

	t.Run("③分片对账截断补标记", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "f")
		parts := dest + ".qparts"
		os.MkdirAll(parts, 0o755)
		writeN(t, dest, 500, 1)                                        // 片 0 已完整合并
		writeN(t, filepath.Join(parts, "p000000"), 400, 2)             // ④ 残留
		writeN(t, filepath.Join(parts, "p000001"), 600, 3)             // > 片大小 → 截断
		writeN(t, filepath.Join(parts, "p000002"), 200, 4)             // 部分片
		os.WriteFile(filepath.Join(parts, "p000002.done"), nil, 0o644) // 假标记（盘上不完整 ≠ 完成）
		m := &QMeta{Total: 1500, W: []int64{500, 0, 500}}              // 记账故意错

		w, mc, mih, ok := ReconcileB(dest, parts, m, segs)
		if !ok {
			t.Fatalf("ok=false")
		}
		if mc != 1 || mih != 0 {
			t.Fatalf("mc=%d mih=%d, want 1/0", mc, mih)
		}
		if got := statSize(filepath.Join(parts, "p000001")); got != 500 {
			t.Errorf("③ 磁盘长度 > 片大小应截断到 500, got %d", got)
		}
		if want := []int64{500, 500, 200}; !equalInt64s(w, want) {
			t.Errorf("w=%v, want %v（W=磁盘长度，不信记账 %v）", w, want, m.W)
		}
		// 磁盘长度 == 片大小 → 置完成标记（防 worker/合并线程互相干等死锁）。
		if _, err := os.Stat(filepath.Join(parts, "p000001.done")); err != nil {
			t.Errorf("③ 全满片应补置完成标记 p000001.done: %v", err)
		}
		// 不完整片的假标记必须清除（合并线程见标记会提前删片）。
		if _, err := os.Stat(filepath.Join(parts, "p000002.done")); !os.IsNotExist(err) {
			t.Errorf("③ 不完整片的假标记 p000002.done 应删除, stat err=%v", err)
		}
	})

	t.Run("①目标超长钳回全合并", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "f")
		parts := dest + ".qparts"
		os.MkdirAll(parts, 0o755)
		writeN(t, dest, 1600, 1) // > total 的病态残留
		m := &QMeta{Total: 1500, W: []int64{500, 500, 500}}

		w, mc, mih, ok := ReconcileB(dest, parts, m, segs)
		if !ok {
			t.Fatalf("ok=false")
		}
		if got := statSize(dest); got != 1500 {
			t.Errorf("目标超长应钳回 total=1500, got %d", got)
		}
		if mc != 3 || mih != 0 {
			t.Errorf("mc=%d mih=%d, want 3/0（钳回后全部已合并）", mc, mih)
		}
		if want := []int64{500, 500, 500}; !equalInt64s(w, want) {
			t.Errorf("w=%v, want %v", w, want)
		}
	})

	t.Run("丢弃判据", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "f")
		if _, _, _, ok := ReconcileB(dest, dest+".qparts", nil, segs); ok {
			t.Errorf("plan 缺失应丢弃（ok=false）")
		}
		bad := &QMeta{Total: 999, W: []int64{500, 500, 500}}
		if _, _, _, ok := ReconcileB(dest, dest+".qparts", bad, segs); ok {
			t.Errorf("total 不符应丢弃（ok=false）")
		}
	})
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRunBResumeAllOnDiskNoMarks：互等死锁场景（§3.5 5 事故注释）——续传
// 恢复时所有分片已完整落盘但**无完成标记**：worker 看 W 满直接跳过、合并
// 线程等标记，互相干等。ReconcileB ③ 补置标记后 runB 必须零请求走完全程。
func TestRunBResumeAllOnDiskNoMarks(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	srv, rec := newRangeServer(t, data)
	dest := filepath.Join(t.TempDir(), "file.bin")
	segs := Plan(total, 8)
	parts := dest + ".qparts"
	if err := os.MkdirAll(parts, 0o755); err != nil {
		t.Fatal(err)
	}
	// 布置：每片完整落盘、无 .done；.qmeta 记账 W 全满（worker 会全部跳过）。
	full := make([]int64, len(segs))
	for i, s := range segs {
		if err := os.WriteFile(filepath.Join(parts, fmt.Sprintf("p%06d", i)), data[s.Start:s.End+1], 0o644); err != nil {
			t.Fatal(err)
		}
		full[i] = s.End - s.Start + 1
	}
	if err := SaveQMeta(dest+".qmeta", &QMeta{Total: total, W: full}); err != nil {
		t.Fatal(err)
	}

	m, err := LoadQMeta(dest + ".qmeta")
	if err != nil {
		t.Fatal(err)
	}
	w, mc, mih, ok := ReconcileB(dest, parts, m, segs)
	if !ok {
		t.Fatalf("ReconcileB 应恢复成功")
	}
	for i := range segs {
		if _, err := os.Stat(filepath.Join(parts, fmt.Sprintf("p%06d.done", i))); err != nil {
			t.Fatalf("③ 盘上完整分片应补置完成标记 p%06d.done（防互等死锁）: %v", i, err)
		}
	}

	tk := newFastTaskB(t, dest, srv.URL, 8, total)
	tk.W = w
	tk.mergeNext, tk.mergeInPart = mc, mih
	var sum int64
	for _, wi := range w {
		sum += wi
	}
	tk.done.Store(sum)
	if err := tk.runB(context.Background()); err != nil {
		t.Fatalf("runB: %v（互等死锁：worker 跳过/合并线程等标记未解开）", err)
	}
	if n := len(rec.snapshot()); n != 0 {
		t.Errorf("服务器收到 %d 个请求, want 0（盘上已完整，worker 应全部跳过）", n)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, data) {
		t.Fatalf("目标文件与源不一致（len got=%d）", len(got))
	}
	if _, err := os.Stat(parts); !os.IsNotExist(err) {
		t.Errorf("分片目录应清空删除, stat err=%v", err)
	}
}

// TestRunBStrictOrder：合并严格按片序（§3.5 3：目标文件必须从 0 连续
// 增长，播放器才能边下边读）。片 0 的响应延迟 300ms，其余片立即完成；
// 采样断言任意时刻目标文件内容 == 源文件前缀（前缀单调增长 ⇔ 严格片序）。
func TestRunBStrictOrder(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	segs := Plan(total, 8)
	seg0 := segs[0].End + 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			t.Errorf("畸形 Range 头 %q: %v", r.Header.Get("Range"), err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if a < seg0 { // 片 0：挂起，让其余 15 片先落盘
			select {
			case <-r.Context().Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[a : b+1])
	}))
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk := newFastTaskB(t, dest, srv.URL, 8, total)

	var violations atomic.Int64
	var midSamples atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if b, err := os.ReadFile(dest); err == nil && len(b) > 0 {
				if !bytes.Equal(b, data[:len(b)]) {
					violations.Add(1) // 目标混入非前缀字节 = 乱序合并
				}
				if int64(len(b)) < total {
					midSamples.Add(1)
				}
			}
			time.Sleep(3 * time.Millisecond)
		}
	}()

	if err := tk.runB(context.Background()); err != nil {
		t.Fatalf("runB: %v", err)
	}
	close(stop)
	<-done

	if v := violations.Load(); v != 0 {
		t.Errorf("目标文件出现非源前缀内容 %d 次（合并未按严格片序）", v)
	}
	if midSamples.Load() == 0 {
		t.Errorf("未采样到中间态目标文件（前缀单调增长证据不足）")
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, data) {
		t.Fatalf("最终目标文件与源不一致（len got=%d）", len(got))
	}
}

// TestRunBResume：kill 模拟——第一轮片 0 恒短体 → 重试耗尽 → 失败保留
// 断点（分片半截在盘、.qmeta 记账可能虚高）；第二轮 ReconcileB 对账后
// 续传：片 0 从盘上断点续（不重复下载）、其余片零请求、最终逐字节一致。
func TestRunBResume(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	segs := Plan(total, 8)
	seg0 := segs[0].End + 1
	dest := filepath.Join(t.TempDir(), "file.bin")

	// 第一轮：片 0 每次只给剩余区间的一半（恒短体 → 片级重试耗尽 → 失败保留）。
	var failP0 atomic.Bool
	failP0.Store(true)
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		if failP0.Load() && a < seg0 {
			w.Write(data[a : a+(b-a+1)/2])
			return
		}
		w.Write(data[a : b+1])
	}))
	t.Cleanup(srv1.Close)

	tk1 := newFastTaskB(t, dest, srv1.URL, 8, total)
	sr := &sleepRecorder{}
	tk1.sleep = sr.sleep // 退避压零（同 TestRunASegRetryExhausted 存量口径）
	err1 := tk1.runB(context.Background())
	if err1 == nil {
		t.Fatalf("第一轮应失败（片 0 重试耗尽）")
	}
	if ClassifyError(err1) != KindRetryExhausted {
		t.Errorf("第一轮错误类别 = %s, want retry-exhausted", ClassifyError(err1))
	}
	if p0 := statSize(filepath.Join(dest+".qparts", "p000000")); p0 <= 0 || p0 >= seg0 {
		t.Fatalf("断点应保留半截片 0（盘上 %d, 片大小 %d）", p0, seg0)
	}
	if _, err := os.Stat(dest + ".qmeta"); err != nil {
		t.Fatalf("失败保留断点：.qmeta 应存在: %v", err)
	}

	// 第二轮：全量服务器 + ReconcileB 恢复。
	failP0.Store(false)
	srv2, rec2 := newRangeServer(t, data)
	m, err := LoadQMeta(dest + ".qmeta")
	if err != nil {
		t.Fatalf("LoadQMeta: %v", err)
	}
	w, mc, mih, ok := ReconcileB(dest, dest+".qparts", m, segs)
	if !ok {
		t.Fatalf("ReconcileB 应恢复成功")
	}
	tk2 := newFastTaskB(t, dest, srv2.URL, 8, total)
	// 快照对账时刻的盘上字节（tk2.W 与 w 共享底层数组，worker 续写会推进它）。
	onDisk := append([]int64(nil), w...)
	tk2.W = w
	tk2.mergeNext, tk2.mergeInPart = mc, mih
	var sum int64
	for _, wi := range w {
		sum += wi
	}
	tk2.done.Store(sum)
	if err := tk2.runB(context.Background()); err != nil {
		t.Fatalf("第二轮 runB: %v", err)
	}

	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, data) {
		t.Fatalf("恢复后目标文件与源不一致（len got=%d）", len(got))
	}
	if _, err := os.Stat(dest + ".qparts"); !os.IsNotExist(err) {
		t.Errorf("分片目录应清空删除, stat err=%v", err)
	}
	// 只重下缺口：每个请求与其片在第一轮已落盘的区间 [Start, Start+w)
	// 零重叠（W=磁盘长度对账后从 S+W 断点续，不重复下载任何字节）。
	for _, rq := range rec2.snapshot() {
		idx := int(rq[0] / seg0)
		if idx >= len(segs) {
			t.Errorf("请求越界片数: %v", rq)
			continue
		}
		if rq[0] < segs[idx].Start+onDisk[idx] {
			t.Errorf("片 %d 重下了已写字节（盘上 %d, 请求起点 %d）: %v",
				idx, onDisk[idx], rq[0]-segs[idx].Start, rq)
		}
	}
	if rq := rec2.snapshot(); len(rq) == 0 {
		t.Errorf("片 0 缺口应至少一次请求")
	} else if first := rq[0]; first[0] <= 0 {
		t.Errorf("片 0 应从盘上断点续传而非从 0 重下: 请求 %v", first)
	}
}

// TestRunBCancelCleanup：用户取消（外部取消令牌）→ 删除 dest + .qmeta +
// 分片目录三件套（§3.10 表「用户主动取消 → 删除」），终态 canceled。
func TestRunBCancelCleanup(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // 挂住所有请求等取消
		case <-r.Context().Done():
			return
		case <-time.After(10 * time.Second):
			w.Write(data)
		}
	}))
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "file.bin")
	tk := newFastTaskB(t, dest, srv.URL, 8, total)

	parent, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tk.runB(parent) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	err := <-done

	if err == nil || !isCancellation(err) {
		t.Fatalf("取消应返回取消类错误, got %v", err)
	}
	if !errors.Is(err, ErrUserCancel) {
		t.Errorf("用户取消应包 ErrUserCancel 锚点, got %v", err)
	}
	if s := taskState(tk.state.Load()); s != stateCanceled {
		t.Errorf("终态 = %s, want canceled", s)
	}
	for _, p := range []string{dest, dest + ".qmeta", dest + ".qparts"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("取消后 %s 应删除, stat err=%v", p, err)
		}
	}
}

// TestRunBDiskSpacePrecheck：模式 B 启动时校验剩余空间 ≥ 待下载量（参数
// 表逐字；不预分配，磁盘满会中途才炸——预检把失败提前到开局）。freeSpace
// 注入化。
func TestRunBDiskSpacePrecheck(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	srv, rec := newRangeServer(t, data)
	dest := filepath.Join(t.TempDir(), "file.bin")
	tk := newFastTaskB(t, dest, srv.URL, 8, total)
	tk.freeSpace = func(string) (uint64, error) { return 1, nil }

	err := tk.runB(context.Background())
	if err == nil {
		t.Fatalf("磁盘预检失败应返回错误")
	}
	if want := "磁盘"; !bytes.Contains([]byte(err.Error()), []byte(want)) {
		t.Errorf("错误应注明磁盘空间不足: %v", err)
	}
	if _, serr := os.Stat(dest + ".qparts"); !os.IsNotExist(serr) {
		t.Errorf("预检失败不应创建分片目录, stat err=%v", serr)
	}
	if n := len(rec.snapshot()); n != 0 {
		t.Errorf("预检失败不应发起任何请求, got %d", n)
	}
	if s := taskState(tk.state.Load()); s != stateError {
		t.Errorf("终态 = %s, want error", s)
	}
}

// TestRunBVerdictFileLen：模式 B 终判（§3.11）：目标文件长度 == total 才算
// 成功（差一个字节都不算）——done 记账满而文件不足时绝不翻成功。
func TestRunBVerdictFileLen(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "f")
	tk := newFastTask(t, dest, "http://unused", 1, 100)
	tk.modeB = true
	tk.done.Store(100) // 记账已满

	if tk.dataComplete() {
		t.Errorf("目标文件不存在时不应判完整")
	}
	os.WriteFile(dest, make([]byte, 99), 0o644)
	if tk.dataComplete() {
		t.Errorf("目标文件 99 字节（差一个字节都不算成功）不应判完整")
	}
	os.WriteFile(dest, make([]byte, 100), 0o644)
	if !tk.dataComplete() {
		t.Errorf("目标文件 == total 应判完整")
	}

	tk.modeB = false // 模式 A 对照：只认记账，与文件长度无关
	os.Remove(dest)
	if !tk.dataComplete() {
		t.Errorf("模式 A done≥total 即完整（预分配下长度无意义）")
	}
}

// TestRunBInvalid：装配错误显式拒绝（同 runA 口径）。
func TestRunBInvalid(t *testing.T) {
	tk := newFastTask(t, filepath.Join(t.TempDir(), "f"), "http://unused", 8, 8<<20)
	tk.Segs = nil
	if err := tk.runB(context.Background()); err == nil {
		t.Errorf("无分片应报错")
	}
	tk2 := newFastTask(t, filepath.Join(t.TempDir(), "g"), "http://unused", 0, 8<<20)
	if err := tk2.runB(context.Background()); err == nil {
		t.Errorf("Threads<1 应报错")
	}
}
