package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quark-downloader/internal/protocol"
)

// Manager（Task 14 收官）集成测试：全部真实 httptest 服务器 + t.TempDir。
// 任务常数（ramp/采样/存档周期）经 Manager.taskTune 压成毫秒级——生产
// 3s/6s 爬坡进测试会拖到分钟级；节流间隔（500ms）不压：TestProgressThrottle
// 验的就是真实节流。

// ---- 测试辅助 ---------------------------------------------------------------

// fastTune 压缩任务常数（Manager.taskTune 注入）。
func fastTune(tk *Task) {
	tk.ramp = 2 * time.Millisecond
	tk.sampleEvery = 5 * time.Millisecond
	tk.qmetaEvery = 20 * time.Millisecond
}

// evLog 线程安全的事件收集器（onEvent 可能在多 goroutine 并发回调）。
type evLog struct {
	mu  sync.Mutex
	evs []Event
	at  []time.Time
}

func (l *evLog) record(e Event) {
	l.mu.Lock()
	l.evs = append(l.evs, e)
	l.at = append(l.at, time.Now())
	l.mu.Unlock()
}

func (l *evLog) snapshot() ([]Event, []time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Event(nil), l.evs...), append([]time.Time(nil), l.at...)
}

// waitForState 轮询 Get 直到任务到达 want 状态（或超时 Fatal）。
func waitForState(t *testing.T, m *Manager, id, want string, timeout time.Duration) Progress {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		p, err := m.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if p.State == want {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待任务 %s 到达 %s 超时（当前 %s, Done=%d/%d）", id, want, p.State, p.Done, p.Total)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForDoneGt 轮询 Get 直到 Done 严格大于 v（抓住下载进行中的窗口）。
func waitForDoneGt(t *testing.T, m *Manager, id string, v int64, timeout time.Duration) Progress {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		p, err := m.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if p.Done > v && p.Done < p.Total {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待任务 %s Done>%d 超时（当前 Done=%d/%d）", id, v, p.Done, p.Total)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// newDripServer 慢速滴灌服务器：Range 请求分块写、每块间隔 per——制造
// 可观察/可暂停的进度窗口（本地回环全速下载毫秒级完成，暂停测试抓不住）。
func newDripServer(t *testing.T, data []byte, block int64, per time.Duration) (*httptest.Server, *rangeRecorder) {
	t.Helper()
	rec := &rangeRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			t.Errorf("畸形 Range 头 %q: %v", r.Header.Get("Range"), err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rec.add(a, b)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		for s := a; s <= b; s += block {
			e := min(s+block-1, b)
			if _, err := w.Write(data[s : e+1]); err != nil {
				return // 客户端取消（暂停/取消在飞请求）
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(per)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// newAuthServer 直链过期形态服务器（§2.10：过期表现 401）：区间完整落在
// [0, okUntil) → 正常 206；否则 401。配小 threads 让 206 片全部完成后再
// 碰 401（worker 队列 FIFO：前几片写完才 pop 到 401 片），断点确定可断言。
func newAuthServer(t *testing.T, data []byte, okUntil int64) (*httptest.Server, *rangeRecorder) {
	t.Helper()
	rec := &rangeRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			t.Errorf("畸形 Range 头 %q: %v", r.Header.Get("Range"), err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rec.add(a, b)
		if b < okUntil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[a : b+1])
			return
		}
		w.WriteHeader(http.StatusUnauthorized) // §2.10：直链过期
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// mustEqualFile 断言 dest 与源逐字节一致。
func mustEqualFile(t *testing.T, dest string, data []byte) {
	t.Helper()
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读回目标文件 %s: %v", dest, err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("目标文件与源不一致（len got=%d want=%d）", len(got), len(data))
	}
}

// assertMonotonicDone 断言事件流 Done 单调不减。
func assertMonotonicDone(t *testing.T, evs []Event) {
	t.Helper()
	for i := 1; i < len(evs); i++ {
		if evs[i].Progress.Done < evs[i-1].Progress.Done {
			t.Errorf("事件流 Done 回退: #%d %d < #%d %d", i, evs[i].Progress.Done, i-1, evs[i-1].Progress.Done)
		}
	}
}

// ---- 简报 Step 1 五个测试 ---------------------------------------------------

// TestManagerEndToEnd 模式 A 端到端：64MB 假文件、512 并发档位、小片。
// 片数推导（§3.3，Ruling E2 公式权威）：eff=512、bySize(64MB)=16 →
// want=max(16,512×32)=16384 → seg=clamp(⌈64MB/16384⌉=4096, 512KB, 8MB)=512KB
// → n=128 片；N=min(512,128,512)=128（「512 并发」被片数钳到 128 路，
// 每片恰一个请求）。断言：完成、逐字节一致、事件流含速度帧且 Done 单调
// （ETA 由 (Total−Done)/SpeedBPS 派生，无独立字段）。
func TestManagerEndToEnd(t *testing.T) {
	total := int64(64 << 20)
	data := testPattern(total)
	srv, rec := newRangeServer(t, data)
	dir := t.TempDir()

	log := &evLog{}
	m := NewManager(Config{Threads: 512, Mode: ModeA, MaxConcurrentTasks: 3, DestDir: dir}, log.record)
	m.taskTune = fastTune

	id, err := m.Submit(protocol.LinkResult{FID: "f1", FileName: "big.bin", Size: total, URL: srv.URL}, "", "", ModeA)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	p := waitForState(t, m, id, "done", 60*time.Second)
	if p.Done != total {
		t.Errorf("终态 Done=%d, want %d", p.Done, total)
	}

	mustEqualFile(t, filepath.Join(dir, "big.bin"), data)

	evs, _ := log.snapshot()
	if len(evs) == 0 {
		t.Fatalf("事件流为空")
	}
	assertMonotonicDone(t, evs)
	haveSpeed := false
	for _, e := range evs {
		if e.Progress.SpeedBPS > 0 {
			haveSpeed = true
		}
		if e.Err != nil {
			t.Errorf("成功任务收到错误事件: %v", e.Err)
		}
	}
	if !haveSpeed {
		t.Errorf("事件流没有任何速度帧（SpeedBPS>0）")
	}

	if n := len(rec.snapshot()); n != 128 {
		t.Errorf("服务器收到 %d 个请求, want 128（每片恰一）", n)
	}
}

// TestManagerModeB 模式 B 装配路径：独立分片 + 边下边合并（runB 复用
// worker 全家桶，本体已由 mergeb_test 覆盖，这里验 Manager 装配接线）。
func TestManagerModeB(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	srv, _ := newRangeServer(t, data)
	dir := t.TempDir()

	m := NewManager(Config{Threads: 8, Mode: ModeB, MaxConcurrentTasks: 2, DestDir: dir}, nil)
	m.taskTune = fastTune

	dest := filepath.Join(dir, "sub", "b.bin") // relDir 落盘（G11）
	id, err := m.Submit(protocol.LinkResult{FID: "f2", FileName: "b.bin", Size: total, URL: srv.URL}, "sub", "", ModeB)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForState(t, m, id, "done", 30*time.Second)
	mustEqualFile(t, dest, data)

	qm, err := LoadQMeta(dest + ".qmeta")
	if err != nil {
		t.Fatalf("完成后 .qmeta 应存在且合法: %v", err)
	}
	for i, w := range qm.W {
		if w == 0 {
			t.Errorf("完成后 W[%d]=0（应全满）", i)
		}
	}
	if _, err := os.Stat(dest + ".qparts"); !os.IsNotExist(err) {
		t.Errorf("分片目录残留: %v", err)
	}
}

// TestLinkRefresh 直链自愈（§2.10 任务级一次一换）：
// 401 服务器 → refresher 换 URL → 续传完成（前 4 片不重下）；
// refresher 返回同 URL → 中止保留断点（.qmeta 在盘、W 记录进度）。
func TestLinkRefresh(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	// threads=1 → 单 worker 串行：片 0..3（前 2MB）逐片写完才请求片 4 碰
	// 401——断点确定可断言（多 worker 时快 worker 提前碰 401 触发级联
	// 取消，前 4 片写不满，断点不确定）。
	const seg = 512 << 10
	const okUntil = 4 * seg
	const threads = 1

	t.Run("换URL续传完成", func(t *testing.T) {
		srvA, recA := newAuthServer(t, data, okUntil)
		srvB, recB := newRangeServer(t, data)
		dir := t.TempDir()

		var refreshes atomic.Int32
		log := &evLog{}
		m := NewManager(Config{Threads: threads, DestDir: dir}, log.record)
		m.taskTune = fastTune
		id, err := m.Submit(protocol.LinkResult{FID: "f3", FileName: "r.bin", Size: total, URL: srvA.URL},
			"", "", ModeA, SubmitOpt{Refresher: func(ctx context.Context) (string, error) {
				refreshes.Add(1)
				return srvB.URL, nil
			}})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitForState(t, m, id, "done", 30*time.Second)

		if n := refreshes.Load(); n != 1 {
			t.Fatalf("refresher 调用 %d 次, want 1（任务级一次一换）", n)
		}
		mustEqualFile(t, filepath.Join(dir, "r.bin"), data)

		// 续传证据：第一轮确实碰过 401；第二轮全部请求都在断点之后。
		saw401 := false
		for _, rq := range recA.snapshot() {
			if rq[1] >= okUntil {
				saw401 = true
			}
		}
		if !saw401 {
			t.Fatalf("第一轮未出现 401（测试场景失效）")
		}
		reqs := recB.snapshot()
		if len(reqs) == 0 {
			t.Fatalf("第二轮服务器无请求")
		}
		for _, rq := range reqs {
			if rq[0] < okUntil {
				t.Errorf("第二轮请求区间 [%d,%d] 落在已完成断点 %d 之内（重复下载）", rq[0], rq[1], okUntil)
			}
		}
		for _, e := range func() []Event { evs, _ := log.snapshot(); return evs }() {
			if e.Err != nil {
				t.Errorf("最终成功任务收到错误事件: %v", e.Err)
			}
		}
	})

	t.Run("同URL中止保断点", func(t *testing.T) {
		srvA, _ := newAuthServer(t, data, okUntil)
		dir := t.TempDir()
		dest := filepath.Join(dir, "r.bin")

		var refreshes atomic.Int32
		log := &evLog{}
		m := NewManager(Config{Threads: threads, DestDir: dir}, log.record)
		m.taskTune = fastTune
		id, err := m.Submit(protocol.LinkResult{FID: "f3", FileName: "r.bin", Size: total, URL: srvA.URL},
			"", "", ModeA, SubmitOpt{Refresher: func(ctx context.Context) (string, error) {
				refreshes.Add(1)
				return srvA.URL, nil // 与旧相同 → 中止（§2.10）
			}})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitForState(t, m, id, "error", 30*time.Second)

		if n := refreshes.Load(); n != 1 {
			t.Fatalf("refresher 调用 %d 次, want 1（同 URL 中止后不再重试）", n)
		}
		if _, err := os.Stat(dest); err != nil {
			t.Fatalf("中止应保留半成品 dest: %v", err)
		}
		qm, err := LoadQMeta(dest + ".qmeta")
		if err != nil {
			t.Fatalf("中止应保留断点 .qmeta: %v", err)
		}
		var sum int64
		for i, w := range qm.W {
			if i < 4 && w != seg {
				t.Errorf("断点 W[%d]=%d, want %d（前 4 片应写满）", i, w, seg)
			}
			sum += w
		}
		if sum != okUntil {
			t.Errorf("断点 ΣW=%d, want %d", sum, okUntil)
		}
		evs, _ := log.snapshot()
		hasErr := false
		for _, e := range evs {
			if e.Err != nil {
				hasErr = true
			}
		}
		if !hasErr {
			t.Errorf("中止终态应有错误事件")
		}
	})
}

// TestPauseResumeCancel 生命周期：暂停→进度冻结→继续（无损续传）；
// 取消→半成品删除（用户意愿，§3.10 表）；排队期取消→起跑前跳过
// （T13 锚点：Scheduler 无 ctx，用 taskID+Cancel 标志表达）。
func TestPauseResumeCancel(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)

	t.Run("暂停冻结与续传", func(t *testing.T) {
		srv, _ := newDripServer(t, data, 64<<10, 20*time.Millisecond)
		dir := t.TempDir()
		m := NewManager(Config{Threads: 8, DestDir: dir}, nil)
		m.taskTune = fastTune

		id, err := m.Submit(protocol.LinkResult{FID: "f4", FileName: "p.bin", Size: total, URL: srv.URL}, "", "", ModeA)
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitForDoneGt(t, m, id, 0, 30*time.Second)

		m.Pause(id)
		p1 := waitForState(t, m, id, "paused", 30*time.Second)

		time.Sleep(150 * time.Millisecond) // 冻结观察窗
		p2, err := m.Get(id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if p2.State != "paused" || p2.Done != p1.Done {
			t.Fatalf("暂停后进度未冻结: 暂停生效 Done=%d, 150ms 后 State=%s Done=%d", p1.Done, p2.State, p2.Done)
		}

		m.Resume(id)
		waitForState(t, m, id, "done", 60*time.Second)
		mustEqualFile(t, filepath.Join(dir, "p.bin"), data) // 无损续传
	})

	t.Run("取消删半成品", func(t *testing.T) {
		srv, _ := newDripServer(t, data, 64<<10, 20*time.Millisecond)
		dir := t.TempDir()
		m := NewManager(Config{Threads: 8, DestDir: dir}, nil)
		m.taskTune = fastTune

		id, err := m.Submit(protocol.LinkResult{FID: "f5", FileName: "c.bin", Size: total, URL: srv.URL}, "", "", ModeA)
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitForDoneGt(t, m, id, 0, 30*time.Second)

		m.Cancel(id)
		p := waitForState(t, m, id, "canceled", 30*time.Second)
		dest := filepath.Join(dir, "c.bin")
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Errorf("取消后半成品应删除, Progress=%+v, stat err=%v", p, err)
		}
		if _, err := os.Stat(dest + ".qmeta"); !os.IsNotExist(err) {
			t.Errorf("取消后 .qmeta 应删除: %v", err)
		}
	})

	t.Run("排队期取消跳过起跑", func(t *testing.T) {
		srv, _ := newDripServer(t, data, 64<<10, 20*time.Millisecond)
		dir := t.TempDir()
		// MaxConcurrentTasks=1：任务 1 占住唯一槽位，任务 2 排队。
		m := NewManager(Config{Threads: 8, MaxConcurrentTasks: 1, DestDir: dir}, nil)
		m.taskTune = fastTune

		id1, err := m.Submit(protocol.LinkResult{FID: "f6", FileName: "q1.bin", Size: total, URL: srv.URL}, "", "", ModeA)
		if err != nil {
			t.Fatalf("Submit1: %v", err)
		}
		id2, err := m.Submit(protocol.LinkResult{FID: "f7", FileName: "q2.bin", Size: total, URL: srv.URL}, "", "", ModeA)
		if err != nil {
			t.Fatalf("Submit2: %v", err)
		}

		m.Cancel(id2) // 排队中取消（goroutine 还阻塞在信号量上）
		waitForState(t, m, id1, "done", 60*time.Second)
		p2 := waitForState(t, m, id2, "canceled", 30*time.Second)

		if p2.Done != 0 {
			t.Errorf("排队取消任务 Done=%d, want 0（从未起跑）", p2.Done)
		}
		if _, err := os.Stat(filepath.Join(dir, "q2.bin")); !os.IsNotExist(err) {
			t.Errorf("排队取消不应留下目标文件: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "q2.bin") + ".qmeta"); !os.IsNotExist(err) {
			t.Errorf("排队取消不应留下 .qmeta: %v", err)
		}
		// Retry 从取消态复活（锁已释放，重新排队正常跑完）。
		m.Retry(id2)
		waitForState(t, m, id2, "done", 60*time.Second)
		mustEqualFile(t, filepath.Join(dir, "q2.bin"), data)
	})
}

// TestSameDestMutex 同路径互斥（§3.13 数据安全非体验）：第二个 Submit
// 被拒「同一文件正在下载中」（不排队）；任务退出释放后同路径可复用。
func TestSameDestMutex(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	srv, _ := newDripServer(t, data, 64<<10, 20*time.Millisecond)
	dir := t.TempDir()
	m := NewManager(Config{Threads: 8, DestDir: dir}, nil)
	m.taskTune = fastTune

	lr := protocol.LinkResult{FID: "f8", FileName: "dup.bin", Size: total, URL: srv.URL}
	id1, err := m.Submit(lr, "", "", ModeA)
	if err != nil {
		t.Fatalf("Submit1: %v", err)
	}
	waitForDoneGt(t, m, id1, 0, 30*time.Second) // 确保任务 1 还在下载中

	_, err = m.Submit(lr, "", "", ModeA)
	if err == nil || !strings.Contains(err.Error(), "同一文件正在下载中") {
		t.Fatalf("同路径第二个 Submit 应被拒, got err=%v", err)
	}

	m.Cancel(id1)
	waitForState(t, m, id1, "canceled", 30*time.Second)

	id3, err := m.Submit(lr, "", "", ModeA) // 锁已释放 → 可复用
	if err != nil {
		t.Fatalf("释放后同路径 Submit 应成功: %v", err)
	}
	waitForState(t, m, id3, "done", 60*time.Second)
	mustEqualFile(t, filepath.Join(dir, "dup.bin"), data)
}

// TestProgressThrottle 进度事件节流：500ms 节流（采样 200ms 不变，测试
// 压采样到 5ms 高频喂入）；相邻事件间隔 ≥500ms−50ms 容差；Done 单调。
func TestProgressThrottle(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	// threads=4 → N=4：16 片×512KB，每片 8 块×40ms=320ms，4 波 ≈1.3s
	// 下载时长——足够观察 ≥2 个节流帧。
	srv, _ := newDripServer(t, data, 64<<10, 40*time.Millisecond)
	dir := t.TempDir()

	log := &evLog{}
	m := NewManager(Config{Threads: 4, DestDir: dir}, log.record)
	m.taskTune = fastTune

	id, err := m.Submit(protocol.LinkResult{FID: "f9", FileName: "t.bin", Size: total, URL: srv.URL}, "", "", ModeA)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForState(t, m, id, "done", 60*time.Second)

	evs, at := log.snapshot()
	assertMonotonicDone(t, evs)

	var runningAt []time.Time
	for i, e := range evs {
		if e.Progress.State == "running" {
			runningAt = append(runningAt, at[i])
		}
	}
	if len(runningAt) < 2 {
		t.Fatalf("running 帧仅 %d 个（下载 ~1.3s / 节流 500ms 至少应有 2 帧）", len(runningAt))
	}
	const minGap = 500*time.Millisecond - 50*time.Millisecond
	for i := 1; i < len(runningAt); i++ {
		if gap := runningAt[i].Sub(runningAt[i-1]); gap < minGap {
			t.Errorf("running 帧间隔 %v < %v（节流失效）", gap, minGap)
		}
	}
}
