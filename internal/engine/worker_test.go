package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 模式 A worker 主循环（§3.5 模式 A 部分）测试：全部走真实 httptest
// Range 服务器 + t.TempDir，Task 常数（ramp/qmeta 周期/采样周期）注入
// 毫秒/微秒级——生产 3s/6s 爬坡进测试会拖到分钟级。

// ---- 测试辅助 ---------------------------------------------------------------

// testPattern 生成 n 字节确定性随机数据（逐字节比对用源文件）。
func testPattern(n int64) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(7, 77))
	for i := 0; i+8 <= len(b); i += 8 {
		binary.LittleEndian.PutUint64(b[i:], r.Uint64())
	}
	for i := len(b) - len(b)%8; i < len(b); i++ {
		b[i] = byte(r.Uint64())
	}
	return b
}

// rangeRecorder 记录服务器收到的每个 Range 请求区间（闭区间 [a,b]）。
type rangeRecorder struct {
	mu   sync.Mutex
	reqs [][2]int64
}

func (r *rangeRecorder) add(a, b int64) {
	r.mu.Lock()
	r.reqs = append(r.reqs, [2]int64{a, b})
	r.mu.Unlock()
}

func (r *rangeRecorder) snapshot() [][2]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reqs)
}

// newRangeServer 起一个支持 Range 的下载服务器（206 + Content-Range，源为
// data）。hang 非 nil 时进入「半途挂起」模式：仅最先到达的 hangFirst 个
// 请求写完前半区间 flush 后阻塞在 hang 上（制造部分片），其余请求正常
// 服务（制造已满片）——断点续传测试在半途取消第一轮。
func newRangeServer(t *testing.T, data []byte, hang chan struct{}, hangFirst int32) (*httptest.Server, *rangeRecorder, *atomic.Int32) {
	t.Helper()
	rec := &rangeRecorder{}
	var hangCount atomic.Int32
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
		if hang != nil && hangCount.Add(1) <= hangFirst {
			mid := a + (b-a+1)/2 // 前半 [a, mid]，flush 后挂起
			w.Write(data[a : mid+1])
			w.(http.Flusher).Flush()
			<-hang
			return // 客户端早已取消，后半不写
		}
		w.Write(data[a : b+1])
	}))
	t.Cleanup(srv.Close)
	return srv, rec, &hangCount
}

// newIgnoreRangeServer 起一个无视 Range 的服务器：一律 200 + 全量 body，
// 供「服务器忽略 Range」语义测试（§3.5：分片写会错位 → 记错误）。
func newIgnoreRangeServer(t *testing.T, data []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK) // 无视 Range，直接 200 全量
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// newFastTask 造一个注入微秒~毫秒级常数的任务（生产 ramp=3s/6s、
// qmetaEvery=1s、sampleEvery=200ms，进测试必须压缩）。
func newFastTask(t *testing.T, dest, url string, threads int, total int64) *Task {
	t.Helper()
	tk := newTask("test", dest, url, "", threads, total)
	tk.ramp = 2 * time.Millisecond
	tk.sampleEvery = time.Millisecond
	tk.qmetaEvery = 5 * time.Millisecond
	return tk
}

// ---- 四个核心用例 -----------------------------------------------------------

// 小文件全流程：8MB/8 线程（Plan → 16 片，Ruling E2：片数直接调 Plan，
// 勿手算）→ 完成后逐字节比对源文件；.qmeta 存在且 W 全满；每片恰一个请求。
func TestRunASmall(t *testing.T) {
	total := int64(8 << 20) // 8MB：bySize(<32MB)=4、eff=8 → want=256 → seg=512KB → 16 片
	data := testPattern(total)
	srv, rec, _ := newRangeServer(t, data, nil, 0)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk := newFastTask(t, dest, srv.URL, 8, total)
	var evMu sync.Mutex
	var events []Progress
	tk.onEvent = func(p Progress) {
		evMu.Lock()
		events = append(events, p)
		evMu.Unlock()
	}
	if err := tk.runA(context.Background()); err != nil {
		t.Fatalf("runA: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读回目标文件: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("目标文件与源不一致（len got=%d want=%d）", len(got), len(data))
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
	if n := len(rec.snapshot()); n != len(tk.Segs) {
		t.Errorf("服务器收到 %d 个请求, want %d（全新任务每片恰一个）", n, len(tk.Segs))
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
}

// 断点续传：第一轮下到一半 cancel → .qmeta 记录部分片 → 重启对账 →
// 只重下未完片（round2 的 Range 区间与第一轮已完成区间 [S, S+W) 零重叠，
// W 已满的片完全没有请求）。
func TestRunAResume(t *testing.T) {
	total := int64(8 << 20)
	const threads = 4 // 16 片、N=4
	const wantHang = 2
	data := testPattern(total)
	block := make(chan struct{})
	srv1, _, hangCount := newRangeServer(t, data, block, wantHang)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk1 := newFastTask(t, dest, srv1.URL, threads, total)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- tk1.runA(ctx) }()

	// 等前 wantHang 个请求挂起在半片（各写 256KB 前半）+ 至少 8 个满片
	// 落账：cancel 时既有「部分片」也有「已满片」，round2 的跳过/续传
	// 才都有证据。
	const halfSeg = 256 << 10 // 512KB 片的前半
	wantDone := int64(wantHang)*halfSeg + int64(8)*(512<<10)
	deadline := time.Now().Add(5 * time.Second)
	for (hangCount.Load() < wantHang || tk1.progress().Done < wantDone) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if tk1.progress().Done < int64(wantHang)*halfSeg {
		t.Fatalf("第一轮没能进入半途状态: hang=%d done=%d", hangCount.Load(), tk1.progress().Done)
	}
	cancel()
	if err := <-runErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("runA 第一轮应因取消返回 context.Canceled, got %v", err)
	}
	close(block) // 放行挂起的 handlers（srv.Close 前必须退出）

	// 预分配证据（§3.5 模式 A 1 / §3.6 ②）：半途文件仍是满尺寸稀疏——
	// 文件长度不是进度，进度只认 W。
	if fi, err := os.Stat(dest); err != nil || fi.Size() != total {
		t.Fatalf("半途文件长度 = (%d, %v), want (%d, nil)（预分配保证）", fi.Size(), err, total)
	}

	m1, err := LoadQMeta(dest + ".qmeta")
	if err != nil {
		t.Fatalf("断点 .qmeta: %v", err)
	}
	partial, full := 0, 0
	for i, s := range tk1.Segs {
		if m1.W[i] == s.End-s.Start+1 {
			full++
		} else if m1.W[i] > 0 {
			partial++
		}
	}
	if partial == 0 || full == 0 {
		t.Fatalf("第一轮应留下部分片与已满片（got full=%d partial=%d）", full, partial)
	}

	// 重启对账（Task 14 的装配职责，此处手动）
	w, ok := ReconcileA(dest, m1, tk1.Segs)
	if !ok {
		t.Fatalf("ReconcileA 拒绝了匹配的断点")
	}
	var done0 int64
	for _, x := range w {
		done0 += x
	}

	srv2, rec2, _ := newRangeServer(t, data, nil, 0)
	tk2 := newFastTask(t, dest, srv2.URL, threads, total)
	tk2.W = w
	tk2.done.Store(done0)
	if err := tk2.runA(context.Background()); err != nil {
		t.Fatalf("runA 第二轮: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("续传完成后目标文件与源不一致")
	}

	// 只重下未完片：round2 请求区间与第一轮已完成区间 [S, S+W) 零重叠
	for _, rq := range rec2.snapshot() {
		a, b := rq[0], rq[1]
		for i, s := range tk1.Segs {
			if m1.W[i] == 0 {
				continue
			}
			if !(b < s.Start || a >= s.Start+m1.W[i]) {
				t.Fatalf("round2 请求 [%d,%d] 与片 %d 已完成区间 [%d,%d) 重叠",
					a, b, i, s.Start, s.Start+m1.W[i])
			}
		}
	}
	// W 已满的片完全没有 round2 请求
	r2 := rec2.snapshot()
	for i, s := range tk1.Segs {
		if m1.W[i] != s.End-s.Start+1 {
			continue
		}
		for _, rq := range r2 {
			if !(rq[1] < s.Start || rq[0] > s.End) {
				t.Errorf("已完成的片 %d 被重下: [%d,%d]", i, rq[0], rq[1])
			}
		}
	}
	// round2 请求数 = 全部片 − 已满片（部分片从断点续传、零片从零下、
	// 满片直接跳过——「只重下未完片」的量化证据）
	if n := len(r2); n != len(tk1.Segs)-full {
		t.Errorf("round2 请求数 = %d, want %d（16 片 − %d 已满片）", n, len(tk1.Segs)-full, full)
	}
}

// 冷启动错峰（§3.5）：ramp 注入 40ms、N=4 → worker w 的起步时刻不早于
// t0 + ramp×w/N（time.After 至少睡满，绝对下界，任意调度下恒成立）。
func TestRunAColdStart(t *testing.T) {
	total := int64(8 << 20)
	const threads = 4 // 16 片 → N=4
	data := testPattern(total)
	srv, _, _ := newRangeServer(t, data, nil, 0)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk := newTask("cold", dest, srv.URL, "", threads, total)
	ramp := 40 * time.Millisecond
	tk.ramp = ramp
	tk.sampleEvery = time.Millisecond
	tk.qmetaEvery = 10 * time.Millisecond

	var mu sync.Mutex
	starts := make([]time.Time, threads)
	t0 := time.Now()
	tk.onWorkerStart = func(w int) {
		mu.Lock()
		starts[w] = time.Now()
		mu.Unlock()
	}
	if err := tk.runA(context.Background()); err != nil {
		t.Fatalf("runA: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for w, at := range starts {
		if at.IsZero() {
			t.Fatalf("worker %d 未起步", w)
		}
	}
	const eps = 5 * time.Millisecond // 测量/调度余量；下界本身由 sleep 语义保证
	for w, at := range starts {
		want := time.Duration(w) * ramp / time.Duration(threads)
		if got := at.Sub(t0); got < want-eps {
			t.Errorf("worker %d 起步过早: got %v, want ≥ %v（错峰未生效）", w, got, want)
		}
	}
	var maxDur time.Duration
	for _, at := range starts {
		maxDur = max(maxDur, at.Sub(t0))
	}
	if tail := time.Duration(threads-1) * ramp / time.Duration(threads); maxDur < tail-eps {
		t.Errorf("末 worker 起步 %v, want ≥ %v（线性分布不存在）", maxDur, tail)
	}
}

// F1（审查）：Threads<1 是装配错误——runA 曾直接 min(Threads, 片数, 512)
// 得 N=0：零 worker、W 全零、firstErr=nil → 终态 done、返回 nil，全零文件
// 被当成功。修复语义：显式返回配置错误（静默钳 1 同样不可取——会掩盖
// 调用方 bug），且在任何副作用（状态迁移/预分配/发请求）之前拒绝。
func TestRunAThreadsInvalid(t *testing.T) {
	total := int64(8 << 20) // Plan(total, 0) 仍产出 16 片（want=max(bySize,0)），守卫可达
	data := testPattern(total)
	srv, rec, _ := newRangeServer(t, data, nil, 0)
	dest := filepath.Join(t.TempDir(), "file.bin")

	for _, threads := range []int{0, -3} {
		t.Run(strconv.Itoa(threads), func(t *testing.T) {
			tk := newFastTask(t, dest, srv.URL, threads, total)
			err := tk.runA(context.Background())
			if err == nil {
				t.Fatalf("Threads=%d 应返回配置错误, got nil（零 worker 静默成功即 F1）", threads)
			}
			if p := tk.progress(); p.State == "done" {
				t.Errorf("Threads=%d 终态 = done, want 非 done（零 worker 不得报成功）", threads)
			}
			if _, statErr := os.Stat(dest); statErr == nil {
				t.Errorf("配置错误应在预分配前拒绝, 目标文件不应被创建")
			}
		})
	}
	if n := len(rec.snapshot()); n != 0 {
		t.Errorf("非法并发档位不应发出任何请求, got %d", n)
	}
}

// F2（审查）：取消与错误的分类竞态——用户 cancel 与服务器干净断连同瞬，
// body 读以 io.EOF 干净收场（CopyBuffer err=nil 但 written<remain），被径直
// 判 errShortBody → 终态 error 而非 canceled。真实网络下该窗口仅纳秒级，
// 故注入 RoundTripper：在「返回 io.EOF 的那次 Read」里精确同步 cancel，
// 确定性构造竞态。修复语义：短体判定前先查 ctx——已取消即按取消归类。
func TestRunACancelVsShortBodyRace(t *testing.T) {
	total := int64(1 << 20) // 2 片 × 512KB；threads=1 → 单 worker 顺序先取片 0
	dest := filepath.Join(t.TempDir(), "file.bin")
	tk := newFastTask(t, dest, "http://race.invalid/x", 1, total)
	ctx, cancel := context.WithCancel(context.Background())

	tk.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			t.Errorf("畸形 Range 头 %q: %v", r.Header.Get("Range"), err)
			return nil, err
		}
		half := (b - a + 1) / 2
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Header:     http.Header{"Content-Range": {fmt.Sprintf("bytes %d-%d/%d", a, b, total)}},
			// 只交前半区间，随后在返回 io.EOF 的同一次 Read 里 cancel：
			// 「干净 EOF + 用户取消」同时成立，恰好压在分类点上。
			Body: &cancelEOFBody{data: make([]byte, half), cancel: cancel},
		}, nil
	})}

	err := tk.runA(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消优先分类: runA 应返回 context.Canceled, got %v", err)
	}
	if p := tk.progress(); p.State != "canceled" {
		t.Errorf("终态 = %q, want canceled（取消竞态不得归为 error）", p.State)
	}
	tk.errMu.Lock()
	first := tk.firstErr
	tk.errMu.Unlock()
	if first != nil {
		t.Errorf("取消竞态不得记 firstErr（应按取消归类）, got %v", first)
	}
}

// roundTripFunc 让测试注入 http.RoundTripper（绕过真实网络，精确控制
// 响应体时序）。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// cancelEOFBody 模拟「服务器干净断连与用户取消同瞬」：交完 data 后的下
// 一次 Read 先 cancel 再返回 io.EOF——io.CopyBuffer 因此以
// (written<remain, err=nil) 收场，确定性落在 errShortBody 分类点上。
type cancelEOFBody struct {
	data   []byte
	off    int
	cancel context.CancelFunc
}

func (b *cancelEOFBody) Read(p []byte) (int, error) {
	if b.off < len(b.data) {
		n := copy(p, b.data[b.off:])
		b.off += n
		return n, nil
	}
	b.cancel()
	return 0, io.EOF
}

func (b *cancelEOFBody) Close() error { return nil }

// 服务器忽略 Range（分片请求回 200）：分片写会从片偏移写坏整个文件——
// 本任务记 errRangeIgnored 错误语义（Task 8 转单线程整下 fallback），
// 且一个字节都不许写（200 的全量 body 绝不能落盘）。
func TestRunARange200Fallback(t *testing.T) {
	total := int64(8 << 20)
	data := testPattern(total)
	srv, hits := newIgnoreRangeServer(t, data)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk := newFastTask(t, dest, srv.URL, 4, total)
	err := tk.runA(context.Background())
	if !errors.Is(err, errRangeIgnored) {
		t.Fatalf("runA 应报「服务器忽略 Range」, got %v", err)
	}
	if p := tk.progress(); p.State != "error" || p.Done != 0 {
		t.Errorf("progress = %+v, want State=error Done=0", p)
	}
	if n := hits.Load(); n == 0 {
		t.Fatalf("服务器未收到任何请求")
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if !bytes.Equal(got, make([]byte, total)) {
		t.Errorf("200 响应体被写入目标文件（预分配全零文件应保持全零）")
	}
}
