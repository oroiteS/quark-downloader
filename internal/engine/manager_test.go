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

// ---- 最终审查修复波（C1/I1/I2/I3/I5/锚测试 A/B） -----------------------------

// TestManagerModeBFreshStartClearsDest C1（最终审查）：模式 B 全新开始
// （.qmeta 缺失/损坏/total 不符 → 对账丢弃）而 dest 已有内容时（典型：
// 同名不同大小的新文件），合并线程 O_APPEND 只追加——新分片全部落到旧
// 文件尾部：dest 永远无法等于 total 且用户旧文件被破坏，Retry 持续错位。
// 修复：起跑前判定全新开始则清 dest/.qmeta/.qparts 残留；完成后逐字节
// 等于新数据。
func TestManagerModeBFreshStartClearsDest(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	srv, _ := newRangeServer(t, data)
	dir := t.TempDir()
	m := NewManager(Config{Threads: 8, Mode: ModeB, MaxConcurrentTasks: 1, DestDir: dir}, nil)
	m.taskTune = fastTune

	dest := filepath.Join(dir, "same.bin")
	// 布置「同名不同大小新文件」现场：dest 有旧内容、有旧分片残留、无 .qmeta。
	writeN(t, dest, 123456, 9)
	os.MkdirAll(dest+".qparts", 0o755)
	writeN(t, filepath.Join(dest+".qparts", "p000000"), 1<<20, 8) // 比新片大的旧分片

	id, err := m.Submit(protocol.LinkResult{FID: "c1", FileName: "same.bin", Size: total, URL: srv.URL}, "", "", ModeB)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForState(t, m, id, "done", 30*time.Second)
	mustEqualFile(t, dest, data) // 逐字节等于新数据（旧内容不得残留/错位）
	if _, err := os.Stat(dest + ".qparts"); !os.IsNotExist(err) {
		t.Errorf("成功后分片目录应清空删除: %v", err)
	}
}

// TestPauseInBuildAttemptWindow I1-a（最终审查，Ruling E8）：Pause 落在
// loop 顶检查之后、setRunning 之前的 buildAttempt 窗口——旧实现无人消费
// （首迭代期 Pause 只设意愿不注入），整个下载跑完才了事，暂停丢失。
// 修复：loop 在 setRunning 之后立即复查暂停/取消意愿。taskTune 钩子把
// Pause 精确注入到 buildAttempt 窗口内（#1 是 Submit 期预览调用，#2 才是
// loop 顶检查已过之后的装配调用）。
func TestPauseInBuildAttemptWindow(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	srv, rec := newDripServer(t, data, 64<<10, 20*time.Millisecond)
	dir := t.TempDir()
	m := NewManager(Config{Threads: 8, DestDir: dir}, nil)

	var tunes atomic.Int32
	m.taskTune = func(tk *Task) {
		fastTune(tk)
		if tunes.Add(1) == 2 {
			m.Pause(tk.ID) // 精确落在 loop 的 buildAttempt 窗口（setRunning 前）
		}
	}

	id, err := m.Submit(protocol.LinkResult{FID: "i1a", FileName: "win.bin", Size: total, URL: srv.URL}, "", "", ModeA)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForState(t, m, id, "paused", 10*time.Second)
	if n := len(rec.snapshot()); n != 0 {
		t.Errorf("窗口内暂停应在起跑前消费（服务器请求数 %d, want 0）", n)
	}
	m.Resume(id)
	waitForState(t, m, id, "done", 60*time.Second)
	mustEqualFile(t, filepath.Join(dir, "win.bin"), data)
}

// TestCancelChainGeneration I1-c（最终审查，Ruling E8）：Resume 后新 loop
// 注册的取消链可能被旧 loop 的 deferred setCancel(nil) 覆盖——此后 Cancel
// 读到 nil 只设标志，在跑 attempt 掐不断，控制面丢失。修复：cancel 链加
// 代际 token，deferred 清空仅同代生效。编排确定性复现：旧 loop 卡在
// suspend 终帧的 onEvent 里（deferred 清空未跑）→ Resume 起新 loop（cap=2
// 有空槽，不等旧 loop 退出）→ 等新 loop 注册完新链（taskTune#3 卡点）→
// 放旧 loop 收尾（旧代码必把新链清成 nil）→ 放新 loop 起跑 → Cancel。
func TestCancelChainGeneration(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	srv, _ := newDripServer(t, data, 64<<10, 20*time.Millisecond)
	dir := t.TempDir()

	pausedFinal := make(chan struct{})
	releaseOld := make(chan struct{})
	newTuned := make(chan struct{})
	releaseNew := make(chan struct{})
	var once sync.Once
	var tunes atomic.Int32

	m := NewManager(Config{Threads: 8, MaxConcurrentTasks: 2, DestDir: dir}, func(e Event) {
		if e.Err == nil && e.Progress.State == "paused" {
			once.Do(func() {
				close(pausedFinal) // 旧 loop 已进入 suspend 的 emitFinal
				<-releaseOld       // 卡住旧 loop：onEvent 不返回 → deferred 清空未跑
			})
		}
	})
	m.taskTune = func(tk *Task) {
		fastTune(tk)
		if tunes.Add(1) == 3 { // #1 Submit 预览 / #2 loop1 尝试 / #3 Resume 后 loop2 尝试
			close(newTuned) // 新 loop 已过取消链注册（setCancelGen），卡在 buildAttempt
			<-releaseNew
		}
	}

	id, err := m.Submit(protocol.LinkResult{FID: "i1c", FileName: "gen.bin", Size: total, URL: srv.URL}, "", "", ModeA)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	p1 := waitForDoneGt(t, m, id, 0, 30*time.Second) // loop1 在跑（代 1 链已注册）

	m.Pause(id)
	<-pausedFinal                                    // 旧 loop 卡在终帧回调
	m.Resume(id)                                     // 新 loop 起跑（不等旧 loop 退出）
	<-newTuned                                       // 新链已注册（代 2）
	close(releaseOld)                                // 旧 loop 收尾：deferred 清 cancel——旧代码覆盖新链为 nil
	close(releaseNew)                                // 新 loop 继续：setRunning → E8 复查 → runA 续传
	waitForDoneGt(t, m, id, p1.Done, 30*time.Second) // 新 attempt 确在跑

	m.Cancel(id) // 必须经新代链掐到在跑 attempt
	waitForState(t, m, id, "canceled", 30*time.Second)
	dest := filepath.Join(dir, "gen.bin")
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("用户取消应删半成品: %v", err)
	}
}

// TestCloseWaitsRacingSubmit I5（最终审查）：Submit 的 wg.Add 必须与 closed
// 复查同处 m.mu 临界区——否则 Close 的 wg.Wait 可从 Submit「已注册、未
// Add」的纳秒窗口穿过：Close 返回后任务 goroutine 才起跑，等齐契约破产。
// 窗口无外部可注入的阻塞点（Unlock→Add 之间是直线代码），只能压力复现：
// 数百轮并发 Submit/Close，断言 Close 返回瞬间已受理任务必已终态。
// 修复后该断言被结构性保证（Add 与注册同临界区，Close 的 closed=true 与
// wg.Wait 同锁序），测试恒绿；修复前为概率红（race 命中窗口时任务仍在
// pending/running）。
func TestCloseWaitsRacingSubmit(t *testing.T) {
	total := int64(1 << 20)
	data := testPattern(total)
	srv, _ := newRangeServer(t, data)
	lr := protocol.LinkResult{FID: "i5", FileName: "race.bin", Size: total, URL: srv.URL}
	dir := t.TempDir()

	type res struct {
		id  string
		err error
	}
	for range 300 {
		m := NewManager(Config{Threads: 4, DestDir: dir}, nil)
		m.taskTune = fastTune
		done := make(chan res, 1)
		go func() {
			id, err := m.Submit(lr, "", "", ModeA)
			done <- res{id, err}
		}()
		m.Close()
		r := <-done
		if r.err != nil {
			continue // 输给 Close（ErrClosed）：合法路径
		}
		p, err := m.Get(r.id) // Close 已返回：立即查询必须是终态
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		switch p.State {
		case "done", "error", "canceled":
		default:
			t.Fatalf("Close 已返回但任务 %s 仍在 %s（Submit 的 wg.Add 竞态：goroutine 未被等齐）", r.id, p.State)
		}
	}
}

// TestRefreshLimitFive 锚测试 A（最终审查）：§2.10 刷新上限逐字「刷新次数
// < 5」——有进度的 401 循环（前 4 片正常、其后恒 401）+ refresher 每次给
// 全新 URL：每次换链重跑都从断点续传后在同一处碰 401，直到第 6 次尝试
// 的刷新请求被拒（used=5 ≥ refreshMax）。断言：任务终态 error、refresher
// 恰被调 5 次、断点保留（.qmeta 在盘且 ΣW=已下字节——零进度形态不在此
// 断言：零进度走 §3.10 4xx 兜底按规格先删后试，无断点可保）。
func TestRefreshLimitFive(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	const seg = 512 << 10
	const okUntil = 4 * seg
	const threads = 1 // 单 worker 串行：断点确定（推导见 TestLinkRefresh）
	srv, _ := newAuthServer(t, data, okUntil)
	dir := t.TempDir()
	dest := filepath.Join(dir, "lim.bin")

	var calls atomic.Int32
	m := NewManager(Config{Threads: threads, DestDir: dir}, nil)
	m.taskTune = fastTune
	id, err := m.Submit(protocol.LinkResult{FID: "a1", FileName: "lim.bin", Size: total, URL: srv.URL},
		"", "", ModeA, SubmitOpt{Refresher: func(ctx context.Context) (string, error) {
			n := calls.Add(1)
			return fmt.Sprintf("%s?refresh=%d", srv.URL, n), nil // 每次全新 URL → 每次都换链重跑
		}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// 等 **Manager 级**终态而非 waitForState：runA 收尾把 Task 级状态置
	// error，自愈换链窗口内（phase 仍 running）Get 会短暂报 "error"——
	// waitForState 会提前返回，把仍在刷新重跑中的任务误判为终态。
	waitTerminal(t, m, id, "error", 30*time.Second)

	if n := calls.Load(); n != 5 {
		t.Fatalf("refresher 恰被调 5 次（第 6 次刷新被拒）, got %d", n)
	}
	qm, err := LoadQMeta(dest + ".qmeta")
	if err != nil {
		t.Fatalf("刷新耗尽终态 error 应保留断点 .qmeta: %v", err)
	}
	var sum int64
	for _, w := range qm.W {
		sum += w
	}
	if sum != okUntil {
		t.Errorf("断点 ΣW=%d, want %d（前 4 片应完整保留）", sum, okUntil)
	}
}

// TestZeroConfigDefaults 锚测试 B（最终审查）：G5 零值默认——
// NewManager(Config{DestDir: dir}) 后 Submit 成功且走 Threads=32 /
// ModeA / 3 任务语义。断言方式说明：规范化（零值→默认）是 NewManager
// 的纯字段变换，字段级直接断言最准；Threads=32 的行为效果（N=min
// (Threads,片数,512) 路数）已有 TestManagerEndToEnd 覆盖，此处补
// MaxConcurrentTasks=3 的行为证据（第 4 个任务必须排队，并发峰值恰 3）
// 与零值配置端到端可用（全部完成 + 逐字节一致）。
func TestZeroConfigDefaults(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	srv, _ := newDripServer(t, data, 64<<10, 20*time.Millisecond)
	dir := t.TempDir()

	m := NewManager(Config{DestDir: dir}, nil) // G5：除 DestDir 外全零值
	if m.cfg.Threads != 32 {
		t.Errorf("零值 Threads 应规范化为 32, got %d", m.cfg.Threads)
	}
	if m.cfg.MaxConcurrentTasks != 3 {
		t.Errorf("零值 MaxConcurrentTasks 应规范化为 3, got %d", m.cfg.MaxConcurrentTasks)
	}
	if m.cfg.Mode != ModeA {
		t.Errorf("零值 Mode 应规范化为 ModeA, got %v", m.cfg.Mode)
	}
	m.taskTune = fastTune

	var ids []string
	for _, name := range []string{"z1.bin", "z2.bin", "z3.bin", "z4.bin"} {
		id, err := m.Submit(protocol.LinkResult{FID: name, FileName: name, Size: total, URL: srv.URL}, "", "", ModeA)
		if err != nil {
			t.Fatalf("Submit(%s): %v", name, err)
		}
		ids = append(ids, id)
	}

	// 行为证据：4 任务 > 默认 3 槽 → 并发运行峰值恰为 3（第 4 个排队）。
	maxRunning := 0
	for {
		running, finished := 0, 0
		for _, id := range ids {
			p, err := m.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			switch p.State {
			case "running":
				running++
			case "done", "error", "canceled":
				finished++
			}
		}
		if running > maxRunning {
			maxRunning = running
		}
		if finished == len(ids) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if maxRunning != 3 {
		t.Errorf("并发运行峰值 = %d, want 3（零值默认 MaxConcurrentTasks=3 的任务语义）", maxRunning)
	}
	for i, name := range []string{"z1.bin", "z2.bin", "z3.bin", "z4.bin"} {
		p, err := m.Get(ids[i])
		if err != nil || p.State != "done" {
			t.Fatalf("零值配置应整体可用: %s State=%s err=%v", name, p.State, err)
		}
		mustEqualFile(t, filepath.Join(dir, name), data)
	}
	m.Close()
}

// waitTerminal 轮询直到任务到达 **Manager 级**终态（phaseTerminal 且
// terminal==want）。与 waitForState 的区别：后者看 Get 的 State 字符串，
// 会被 attempt 级的瞬态污染（runA 失败收尾置 Task.state=error，自愈换链
// 窗口内 Get 短暂报 error 而 phase 仍是 running）。
func waitTerminal(t *testing.T, m *Manager, id, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		mt := m.lookup(id)
		if mt == nil {
			t.Fatalf("waitTerminal: 任务 %s 不存在", id)
		}
		mt.mu.Lock()
		ok := mt.phase == phaseTerminal && mt.terminal == want
		state := mt.terminal
		mt.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待任务 %s 到达终态 %s 超时（当前 terminal=%q）", id, want, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---- 审查修复 F1/F2 回归 -----------------------------------------------------

// TestRefreshCancelChain F1（审查）：Refresher 的 ctx 必须挂在任务取消链上。
// 回归形态：挂起的 refresher（等 ctx.Done）在 Cancel 后被真掐——否则任务
// goroutine 永久阻塞且持有信号量槽位，Cancel 掐不到、槽位耗尽。
func TestRefreshCancelChain(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	const seg = 512 << 10
	const okUntil = 4 * seg
	const threads = 1 // 单 worker 串行：断点确定（推导见 TestLinkRefresh）

	t.Run("Cancel掐到刷新ctx", func(t *testing.T) {
		srvA, _ := newAuthServer(t, data, okUntil)
		dir := t.TempDir()
		entered := make(chan struct{})
		cancelled := make(chan struct{})
		m := NewManager(Config{Threads: threads, MaxConcurrentTasks: 1, DestDir: dir}, nil)
		m.taskTune = fastTune
		id, err := m.Submit(protocol.LinkResult{FID: "fc1", FileName: "rc.bin", Size: total, URL: srvA.URL},
			"", "", ModeA, SubmitOpt{Refresher: func(ctx context.Context) (string, error) {
				close(entered)
				<-ctx.Done() // 挂起：取消链真通到这里才会醒
				close(cancelled)
				return "", ctx.Err()
			}})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		<-entered // 已进入刷新窗口（runA 已因 401 收尾）
		m.Cancel(id)
		select {
		case <-cancelled:
		case <-time.After(10 * time.Second):
			t.Fatalf("Cancel 未传导到 refresher ctx（挂起 10s 未被掐——取消链断裂，槽位被永久占用）")
		}
		// I3（最终审查）：刷新窗口内用户取消——终态按 canceled 记（原实现
		// 记 error）；删除语义由 finishCanceled 兜底（§3.10 用户主动取消
		// → 删 dest + .qmeta；与 Close 停机的保留分支相对）。
		waitForState(t, m, id, "canceled", 30*time.Second)
		rcDest := filepath.Join(dir, "rc.bin")
		if _, err := os.Stat(rcDest); !os.IsNotExist(err) {
			t.Errorf("刷新窗口用户取消应删半成品: %v", err)
		}
		if _, err := os.Stat(rcDest + ".qmeta"); !os.IsNotExist(err) {
			t.Errorf("刷新窗口用户取消应删 .qmeta: %v", err)
		}
		// 槽位释放证明：1 槽 Manager 还能接新任务并跑完
		srvB, _ := newRangeServer(t, data)
		id2, err := m.Submit(protocol.LinkResult{FID: "fc2", FileName: "rc2.bin", Size: total, URL: srvB.URL}, "", "", ModeA)
		if err != nil {
			t.Fatalf("槽位应已释放: Submit2: %v", err)
		}
		waitForState(t, m, id2, "done", 60*time.Second)
		mustEqualFile(t, filepath.Join(dir, "rc2.bin"), data)
	})

	t.Run("超时层掐断刷新", func(t *testing.T) {
		srvA, _ := newAuthServer(t, data, okUntil)
		dir := t.TempDir()
		m := NewManager(Config{Threads: threads, DestDir: dir}, nil)
		m.taskTune = fastTune
		m.refreshTimeout = 50 * time.Millisecond // 注入工程超时（默认 60s）
		id, err := m.Submit(protocol.LinkResult{FID: "fc3", FileName: "rt.bin", Size: total, URL: srvA.URL},
			"", "", ModeA, SubmitOpt{Refresher: func(ctx context.Context) (string, error) {
				<-ctx.Done() // 挂死不返回：只有超时层能掐
				return "", ctx.Err()
			}})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		// 两层任一到期 → 取不到新链 → 中止保留断点（F1 判定）
		waitForState(t, m, id, "error", 30*time.Second)
		if _, err := LoadQMeta(filepath.Join(dir, "rt.bin") + ".qmeta"); err != nil {
			t.Fatalf("超时掐断刷新应中止保留断点: %v", err)
		}
	})
}

// TestEviction F2（审查）：登记表超上限按终态时间最早淘汰**已终态**条目
// （Cookie/URL 随条目释放）；被淘汰后 Get=ErrTaskNotFound 且 Retry 无效；
// 存活任务（排队/运行/暂停）绝不被淘汰。
func TestEviction(t *testing.T) {
	t.Run("终态最早者先淘汰", func(t *testing.T) {
		total := int64(1 << 20)
		data := testPattern(total)
		srv, _ := newRangeServer(t, data)
		dir := t.TempDir()
		m := NewManager(Config{Threads: 4, MaxConcurrentTasks: 1, DestDir: dir}, nil)
		m.taskTune = fastTune
		m.maxTasks = 2 // 注入上限（未导出字段；默认 1024）

		var ids []string
		for _, name := range []string{"e1.bin", "e2.bin", "e3.bin"} {
			id, err := m.Submit(protocol.LinkResult{FID: name, FileName: name, Size: total, URL: srv.URL}, "", "", ModeA)
			if err != nil {
				t.Fatalf("Submit(%s): %v", name, err)
			}
			waitForState(t, m, id, "done", 30*time.Second) // 单槽串行 → 终态时间严格有序
			ids = append(ids, id)
		}
		// 第三个终态把登记表推过上限 2 → 最早终态的 e1 被淘汰
		if _, err := m.Get(ids[0]); !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("最早终态任务应被淘汰: Get err=%v", err)
		}
		for _, id := range ids[1:] {
			p, err := m.Get(id)
			if err != nil {
				t.Fatalf("存活任务不应被淘汰: %v", err)
			}
			if p.State != "done" {
				t.Errorf("存活任务 State=%s, want done", p.State)
			}
		}
		m.Retry(ids[0]) // 已淘汰：无复活（复活职责在 web 层 §6.2 持久化）
		if _, err := m.Get(ids[0]); !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("淘汰后 Retry 不应复活: %v", err)
		}
	})

	t.Run("运行中不被淘汰且Submit触发淘汰", func(t *testing.T) {
		total := int64(8 << 20)
		data := testPattern(total)
		drip, _ := newDripServer(t, data, 64<<10, 50*time.Millisecond) // 慢速：制造持续 running
		fast, _ := newRangeServer(t, data)
		dir := t.TempDir()
		m := NewManager(Config{Threads: 4, MaxConcurrentTasks: 3, DestDir: dir}, nil)
		m.taskTune = fastTune
		m.maxTasks = 2

		idR, err := m.Submit(protocol.LinkResult{FID: "fr", FileName: "run.bin", Size: total, URL: drip.URL}, "", "", ModeA)
		if err != nil {
			t.Fatalf("Submit(run): %v", err)
		}
		waitForDoneGt(t, m, idR, 0, 30*time.Second)

		submitFast := func(name string) string {
			t.Helper()
			id, err := m.Submit(protocol.LinkResult{FID: name, FileName: name, Size: total, URL: fast.URL}, "", "", ModeA)
			if err != nil {
				t.Fatalf("Submit(%s): %v", name, err)
			}
			waitForState(t, m, id, "done", 30*time.Second)
			return id
		}
		idA := submitFast("a.bin") // len=2（run+a）
		idB := submitFast("b.bin") // len=3 > 2 → 终态最早者 a 淘汰；running 幸存
		if _, err := m.Get(idA); !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("超限后终态最早者 a 应被淘汰: %v", err)
		}
		if p, err := m.Get(idR); err != nil || p.State != "running" {
			t.Fatalf("运行中任务不应被淘汰/受影响: err=%v State=%s", err, p.State)
		}
		idC := submitFast("c.bin") // Submit 触发路径：注册即 len=3 → 淘汰终态 b
		if _, err := m.Get(idB); !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("Submit 触发淘汰应淘汰终态最早者 b: %v", err)
		}
		if p, err := m.Get(idR); err != nil || p.State != "running" {
			t.Fatalf("淘汰后运行中任务仍不应受影响: err=%v State=%s", err, p.State)
		}
		if p, err := m.Get(idC); err != nil || p.State != "done" {
			t.Fatalf("最新终态任务应存活: err=%v State=%s", err, p.State)
		}
		// 存活任务收口不受淘汰影响
		m.Cancel(idR)
		waitForState(t, m, idR, "canceled", 30*time.Second)
	})
}

// TestManagerClose F2（审查）+ E6（最终审查）：Close=停止全部任务并
// **保留断点**（删除只属于用户显式 Cancel）——运行中经挂起类收尾走保留
// 分支、排队/暂停直接置终态保留；Close 返回即全部任务 goroutine 已退出；
// 幂等；Close 后 Submit 报 ErrClosed、Retry/Resume 不复活。保留的断点
// 必须可续：新 Manager（模拟进程重启）Submit 同文件从断点无损续传完成
// （D9/G4 重启续传愿景）。
func TestManagerClose(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	drip, _ := newDripServer(t, data, 64<<10, 50*time.Millisecond)
	dir := t.TempDir()
	m := NewManager(Config{Threads: 4, MaxConcurrentTasks: 1, DestDir: dir}, nil)
	m.taskTune = fastTune

	submit := func(name string) string {
		t.Helper()
		id, err := m.Submit(protocol.LinkResult{FID: name, FileName: name, Size: total, URL: drip.URL}, "", "", ModeA)
		if err != nil {
			t.Fatalf("Submit(%s): %v", name, err)
		}
		return id
	}
	idR := submit("cl-r.bin")
	waitForDoneGt(t, m, idR, 0, 30*time.Second) // running（唯一槽位被占）
	idQ1 := submit("cl-q1.bin")                 // 排队在信号量上
	idQ2 := submit("cl-q2.bin")

	m.Close() // 返回即：全部任务已停、全部 goroutine 已退出、断点全保留

	for _, id := range []string{idR, idQ1, idQ2} {
		p, err := m.Get(id)
		if err != nil {
			t.Fatalf("Close 后 Get(%s): %v", id, err)
		}
		if p.State != "canceled" {
			t.Fatalf("Close 后 %s State=%s, want canceled（Close 应已等齐 goroutine）", id, p.State)
		}
	}
	// E6 翻转：半成品保留（Close 不删断点——删除只属于用户显式 Cancel）。
	dest := filepath.Join(dir, "cl-r.bin")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("Close 应保留半成品 dest: %v", err)
	}
	qm, err := LoadQMeta(dest + ".qmeta")
	if err != nil {
		t.Fatalf("Close 应保留断点 .qmeta: %v", err)
	}

	if _, err := m.Submit(protocol.LinkResult{FID: "x", FileName: "x.bin", Size: total, URL: drip.URL}, "", "", ModeA); !errors.Is(err, ErrClosed) {
		t.Fatalf("Close 后 Submit 应报 ErrClosed, got %v", err)
	}
	m.Close() // 幂等：二次 Close 不挂不炸
	m.Retry(idQ1)
	m.Resume(idR)
	if p, _ := m.Get(idQ1); p.State != "canceled" {
		t.Fatalf("Close 后 Retry 不应复活: State=%s", p.State)
	}

	// 重启续传证明（D9/G4）：新 Manager 同目录 Submit，从保留断点无损续传
	// （每个请求都从对应片的盘上断点 S+W 之后开始——不重复下载任何字节）。
	var sum int64
	for _, w := range qm.W {
		sum += w
	}
	if sum == 0 {
		t.Fatalf("布置前提失效：Close 时任务应有非零断点")
	}
	segs := Plan(total, 4)
	fast, recFast := newRangeServer(t, data)
	m2 := NewManager(Config{Threads: 4, MaxConcurrentTasks: 1, DestDir: dir}, nil)
	m2.taskTune = fastTune
	id2, err := m2.Submit(protocol.LinkResult{FID: "cl-r", FileName: "cl-r.bin", Size: total, URL: fast.URL}, "", "", ModeA)
	if err != nil {
		t.Fatalf("重启 Submit: %v", err)
	}
	waitForState(t, m2, id2, "done", 60*time.Second)
	mustEqualFile(t, dest, data)
	for _, rq := range recFast.snapshot() {
		idx := 0
		for idx+1 < len(segs) && segs[idx+1].Start <= rq[0] {
			idx++
		}
		if rq[0] < segs[idx].Start+qm.W[idx] {
			t.Errorf("重启后重下了已写字节（片 %d 断点 %d, 请求起点 %d）", idx, qm.W[idx], rq[0]-segs[idx].Start)
		}
	}
	m2.Close()
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

// ---- Task 8（E7 预算接线）：SubmitOpt.Threads 逐任务档位 ----------------------

// inflightTracker 在飞连接计数：drip handler 进入 +1、退出 -1，max 记录
// 历史峰值——「单任务并发上限」的行为证据（在飞下载数 ≤ worker 数 =
// min(Threads, 片数, 512)；monitor/监视线程不发请求，服务器收到的全是
// worker 的 Range GET）。
type inflightTracker struct {
	mu  sync.Mutex
	cur int
	max int
}

func (tr *inflightTracker) enter() {
	tr.mu.Lock()
	tr.cur++
	if tr.cur > tr.max {
		tr.max = tr.cur
	}
	tr.mu.Unlock()
}

func (tr *inflightTracker) exit() {
	tr.mu.Lock()
	tr.cur--
	tr.mu.Unlock()
}

func (tr *inflightTracker) peak() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.max
}

// newDripServerInflight 带在飞连接计数的 drip server（滴灌节奏同
// newDripServer；4MB 源在档位 ≤8 下切 8 片 × 512KB，每片 8 块 × 20ms，
// 慢速窗口保证多 worker 同时在飞可观察）。
func newDripServerInflight(t *testing.T, data []byte, block int64, per time.Duration) (*httptest.Server, *inflightTracker, *rangeRecorder) {
	t.Helper()
	rec := &rangeRecorder{}
	tr := &inflightTracker{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.enter()
		defer tr.exit()
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
			if e < b {
				time.Sleep(per) // 块间隔滴灌；最后一块后不睡——收尾尾巴
				// 会被计入在飞峰值（worker 读满片长即换片，不等 handler
				// 退出），系统性抬高 peak、打破「峰值 ≤ worker 数」上界。
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, tr, rec
}

// TestSubmitOptThreads 逐任务并发档位（Task 8 / E7）四个断面：
//  1. per-task 生效：cfg=8 而 opt=2 → 在飞连接峰值恰 2（档位压过 cfg）；
//  2. 0 回落 cfg.Threads：opt=0、cfg=3 → 峰值 ≤3 且确实多路（≥2——0 被
//     穿透直传 newTask 会得 n=1 单连接）；
//  3. >512 钳 512（effMax）：opt=513 → 任务档位恰 512、闸门容量
//     min(512, 片数)（4MB 源仅 8 片 <512，513/512 行为等价——断装配值）；
//  4. <0 拒绝：Submit 同步返回 ErrInvalidThreads（goroutine 未启动、
//     未占 dest 锁）——同路径合法任务随即成功证失败路径干净。
func TestSubmitOptThreads(t *testing.T) {
	t.Run("PerTaskCap", func(t *testing.T) {
		total := int64(4 << 20)
		data := testPattern(total)
		srv, tr, rec := newDripServerInflight(t, data, 64<<10, 20*time.Millisecond)
		dir := t.TempDir()
		m := NewManager(Config{Threads: 8, Mode: ModeA, MaxConcurrentTasks: 3, DestDir: dir}, nil)
		m.taskTune = fastTune
		defer m.Close()

		id, err := m.Submit(protocol.LinkResult{FID: "pt", FileName: "pt.bin", Size: total, URL: srv.URL}, "", "", ModeA, SubmitOpt{Threads: 2})
		if err != nil {
			t.Fatalf("Submit(Threads:2): %v", err)
		}
		p := waitForState(t, m, id, "done", 60*time.Second)
		if p.Done != total {
			t.Errorf("终态 Done=%d, want %d", p.Done, total)
		}
		mustEqualFile(t, filepath.Join(dir, "pt.bin"), data)
		if peak := tr.peak(); peak != 2 {
			t.Errorf("在飞连接峰值 = %d, want 2（cfg=8 应被 per-task 档位 2 压制）", peak)
		}
		if n := len(rec.snapshot()); n < 2 {
			t.Errorf("服务器仅收到 %d 个请求（want ≥2，8 片多路分发）", n)
		}
	})

	t.Run("ZeroFallsBackToCfg", func(t *testing.T) {
		total := int64(4 << 20)
		data := testPattern(total)
		srv, tr, _ := newDripServerInflight(t, data, 64<<10, 20*time.Millisecond)
		dir := t.TempDir()
		m := NewManager(Config{Threads: 3, Mode: ModeA, MaxConcurrentTasks: 3, DestDir: dir}, nil)
		m.taskTune = fastTune
		defer m.Close()

		id, err := m.Submit(protocol.LinkResult{FID: "z0", FileName: "z0.bin", Size: total, URL: srv.URL}, "", "", ModeA, SubmitOpt{Threads: 0})
		if err != nil {
			t.Fatalf("Submit(Threads:0): %v", err)
		}
		waitForState(t, m, id, "done", 60*time.Second)
		mustEqualFile(t, filepath.Join(dir, "z0.bin"), data)
		peak := tr.peak()
		if peak > 3 {
			t.Errorf("在飞连接峰值 = %d, want ≤3（Threads=0 应回落 cfg.Threads=3）", peak)
		}
		if peak < 2 {
			t.Errorf("在飞连接峰值 = %d, want ≥2（Threads=0 不应穿透为单连接档位）", peak)
		}
	})

	t.Run("ClampToEffMax", func(t *testing.T) {
		total := int64(4 << 20)
		data := testPattern(total)
		srv, _, _ := newDripServerInflight(t, data, 64<<10, 20*time.Millisecond)
		dir := t.TempDir()
		m := NewManager(Config{Threads: 2, Mode: ModeA, MaxConcurrentTasks: 3, DestDir: dir}, nil)
		m.taskTune = fastTune
		defer m.Close()

		id, err := m.Submit(protocol.LinkResult{FID: "c513", FileName: "c513.bin", Size: total, URL: srv.URL}, "", "", ModeA, SubmitOpt{Threads: 513})
		if err != nil {
			t.Fatalf("Submit(Threads:513): %v", err)
		}
		waitForState(t, m, id, "done", 60*time.Second)
		mustEqualFile(t, filepath.Join(dir, "c513.bin"), data)

		mt := m.lookup(id)
		mt.mu.Lock()
		tk, mtThreads := mt.task, mt.threads
		mt.mu.Unlock()
		if mtThreads != effMax {
			t.Errorf("登记档位 mt.threads = %d, want %d（513 应钳 effMax）", mtThreads, effMax)
		}
		if tk.Threads != effMax {
			t.Errorf("任务档位 tk.Threads = %d, want %d（513 应钳 effMax）", tk.Threads, effMax)
		}
		g, ok := tk.gate.(*Gate)
		if !ok {
			t.Fatalf("闸门未装配为 *Gate（got %T）", tk.gate)
		}
		if want := min(effMax, len(tk.Segs)); g.n != want {
			t.Errorf("闸门容量 = %d, want %d（min(effMax, 片数)）", g.n, want)
		}
	})

	t.Run("NegativeRejected", func(t *testing.T) {
		total := int64(4 << 20)
		data := testPattern(total)
		srv, _, _ := newDripServerInflight(t, data, 64<<10, 20*time.Millisecond)
		dir := t.TempDir()
		m := NewManager(Config{Threads: 2, Mode: ModeA, MaxConcurrentTasks: 3, DestDir: dir}, nil)
		m.taskTune = fastTune
		defer m.Close()

		lr := protocol.LinkResult{FID: "neg", FileName: "neg.bin", Size: total, URL: srv.URL}
		if _, err := m.Submit(lr, "", "", ModeA, SubmitOpt{Threads: -1}); !errors.Is(err, ErrInvalidThreads) {
			t.Fatalf("Submit(Threads:-1) err = %v, want ErrInvalidThreads（启动 goroutine 前同步拒绝）", err)
		}
		// 失败不留痕：dest 锁未占用——同路径合法任务随即成功并跑完。
		id, err := m.Submit(lr, "", "", ModeA)
		if err != nil {
			t.Fatalf("负档位拒绝后同路径重提: %v（dest 锁疑似被失败路径占用）", err)
		}
		waitForState(t, m, id, "done", 60*time.Second)
		mustEqualFile(t, filepath.Join(dir, "neg.bin"), data)
	})
}
