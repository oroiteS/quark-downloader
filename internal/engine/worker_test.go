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
// data）。原「半途挂起」模式（Task 7 断点续传用 cancel 制造部分片）已
// 随 Task 8 语义演进移除：用户取消现在删除半成品（§3.10 表），续传测试
// 改用「恒短体 → 重试耗尽 → 保留断点」制造第一轮中止。
func newRangeServer(t *testing.T, data []byte) (*httptest.Server, *rangeRecorder) {
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
		w.Write(data[a : b+1])
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// newIgnoreRangeServer 起一个无视 Range 的服务器：一律 200 + 全量 body，
// 供「服务器忽略 Range」语义测试（§3.5：分片写会错位 → 记错误）。

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
	srv, rec := newRangeServer(t, data)
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

// 断点续传：第一轮下到一半中止 → .qmeta 记录部分片 → 重启对账 →
// 只重下未完片（round2 的 Range 区间与第一轮已完成区间 [S, S+W) 零重叠，
// W 已满的片完全没有请求）。
// 中止方式（Task 8 起语义）：片 0/1 恒短体 → 重试耗尽 → 任务失败——
// §3.10 分类表「片重试耗尽 → 保留半成品（见完整性终判）」正是续传
// 断点的来源；用户取消已改为删除半成品，不能再用于造断点（生死断言
// 见 TestFinalizeCleanup）。
func TestRunAResume(t *testing.T) {
	total := int64(8 << 20)
	const threads = 4 // 16 片、N=4
	const wantExhausted = 2
	data := testPattern(total)
	rec1 := &rangeRecorder{}
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			t.Errorf("畸形 Range 头 %q: %v", r.Header.Get("Range"), err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rec1.add(a, b)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		if a < wantExhausted*(512<<10) { // 片 0/1：恒写一半即断（短体→耗尽）
			w.Write(data[a : a+(b-a+1)/2+1])
			return
		}
		w.Write(data[a : b+1])
	}))
	t.Cleanup(srv1.Close)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk1 := newFastTask(t, dest, srv1.URL, threads, total)
	sr := &sleepRecorder{}
	tk1.sleep = sr.sleep // 退避压零：耗尽判定本身不等真实退避
	err := tk1.runA(context.Background())
	if k := ClassifyError(err); k != KindRetryExhausted {
		t.Fatalf("第一轮应因片 0/1 重试耗尽失败, got kind=%q err=%v", k, err)
	}

	// 预分配证据（§3.5 模式 A 1 / §3.6 ②）：半途文件仍是满尺寸稀疏——
	// 文件长度不是进度，进度只认 W。
	if fi, statErr := os.Stat(dest); statErr != nil || fi.Size() != total {
		t.Fatalf("半途文件长度 = (%d, %v), want (%d, nil)（预分配保证）", fi.Size(), statErr, total)
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

	srv2, rec2 := newRangeServer(t, data)
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
	srv, _ := newRangeServer(t, data)
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
	srv, rec := newRangeServer(t, data)
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

// 服务器忽略 Range（分片请求回 200）的语义已由 Task 8（§3.10）接手：
// 不再是致命报错，而是删除半成品后转单线程全量——见下方
// TestRunARangeUnsupportedFallback（新语义测试）。

// ---- Task 8（§3.10 片级重试与错误分类）测试 ----------------------------------

// 片级重试退避等待的注入桩：立即返回并记录时长（验证 BackoffSeg 公式
// 实际生效区间），或阻塞在 ctx 上（验证等待期可被抢占/取消打断）。
type sleepRecorder struct {
	mu      sync.Mutex
	durs    []time.Duration
	block   bool          // true：阻塞到 ctx 结束（等待期抢占测试用）
	entered chan struct{} // block 模式下首次进入的通知（一缓冲）
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.durs = append(s.durs, d)
	s.mu.Unlock()
	if s.block {
		if s.entered != nil {
			select {
			case s.entered <- struct{}{}:
			default:
			}
		}
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (s *sleepRecorder) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.durs)
}

// dlReqInfo 服务器侧记录的一次请求画像（头序断言用）。
type dlReqInfo struct {
	hasRange, hasCookie, hasReferer bool
	ua                              string
}

// 片重试断点续（§3.10「从 S+W 断点继续」）：片 0 首次请求短体（写一半
// 即断），片级重试发出的第二个请求起点必须是 S+W（已写字节不重下），
// 服务器端以 Range 起点为证。退避时长必须落在 BackoffSeg 公式区间内。
func TestRunASegRetryResume(t *testing.T) {
	total := int64(2 << 20) // 2MB → 4 片 × 512KB
	const threads = 1
	data := testPattern(total)
	rec := &rangeRecorder{}
	var seg0Hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			t.Errorf("畸形 Range 头 %q: %v", r.Header.Get("Range"), err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rec.add(a, b)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, total))
		w.WriteHeader(http.StatusPartialContent)
		if a == 0 && seg0Hits.Add(1) == 1 { // 片 0 首次：前半即断（短体）
			half := (b - a + 1) / 2
			w.Write(data[a : a+half+1])
			return
		}
		w.Write(data[a : b+1])
	}))
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk := newFastTask(t, dest, srv.URL, threads, total)
	sr := &sleepRecorder{}
	tk.sleep = sr.sleep
	if err := tk.runA(context.Background()); err != nil {
		t.Fatalf("runA: %v（短体应经片级重试自愈）", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("重试续传完成后目标文件与源不一致")
	}

	// 断点续的量化证据：片 0 恰两个请求，第二个起点 = S + 首次已写字节
	half := (tk.Segs[0].End - tk.Segs[0].Start + 1) / 2
	reqs := rec.snapshot()
	var seg0 [][2]int64
	for _, rq := range reqs {
		if rq[0] >= tk.Segs[0].Start && rq[1] <= tk.Segs[0].End {
			seg0 = append(seg0, rq)
		}
	}
	if len(seg0) != 2 {
		t.Fatalf("片 0 请求数 = %d, want 2（首次短体 + 断点续）: %v", len(seg0), seg0)
	}
	if seg0[1][0] != tk.Segs[0].Start+half+1 {
		t.Errorf("断点续请求起点 = %d, want %d（S+W, 不重复下载）", seg0[1][0], tk.Segs[0].Start+half+1)
	}
	// 总请求数：4 片各一 + 片 0 补一 = 5
	if n := len(reqs); n != len(tk.Segs)+1 {
		t.Errorf("总请求数 = %d, want %d", n, len(tk.Segs)+1)
	}
	// 退避公式在 worker 内实际生效：恰一次等待，时长 ∈ [500ms×0.6, 500ms×1.4)
	if n := sr.count(); n != 1 {
		t.Fatalf("退避等待次数 = %d, want 1", n)
	}
	sr.mu.Lock()
	d := sr.durs[0]
	sr.mu.Unlock()
	if d < BackoffSeg(0, 0) || d >= BackoffSeg(0, 1) {
		t.Errorf("退避时长 %v 不在公式区间 [%v, %v)", d, BackoffSeg(0, 0), BackoffSeg(0, 1))
	}
}

// 片重试耗尽入 errors（§3.10「片重试耗尽 / 任务失败」行）：单片恒短体，
// 总尝试 ≤4 次（§3.5「片级重试循环（最多 4 次）」），耗尽后任务失败、
// firstErr 带 KindRetryExhausted 且链上可挖到 errShortBody；半成品保留
// （终判归 Task 11，本任务只负责不计成成功）。
func TestRunASegRetryExhausted(t *testing.T) {
	total := int64(500_000) // < 512KB 首片 → 单片任务
	data := testPattern(total)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			t.Errorf("畸形 Range 头: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, total))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[a : a+(b-a+1)/2+1]) // 恒写一半即断
	}))
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk := newFastTask(t, dest, srv.URL, 1, total)
	sr := &sleepRecorder{}
	tk.sleep = sr.sleep
	err := tk.runA(context.Background())
	if err == nil {
		t.Fatalf("恒短体任务应失败, got nil")
	}
	if k := ClassifyError(err); k != KindRetryExhausted {
		t.Errorf("错误类别 = %q, want %q（重试耗尽）, err=%v", k, KindRetryExhausted, err)
	}
	if !errors.Is(err, errShortBody) {
		t.Errorf("耗尽错误应保留底层原因 errShortBody, got %v", err)
	}
	if n := hits.Load(); n != 4 {
		t.Errorf("尝试次数 = %d, want 4（片级重试循环最多 4 次）", n)
	}
	if n := sr.count(); n != 3 {
		t.Errorf("退避等待次数 = %d, want 3（4 次尝试之间 3 次退避）", n)
	}
	if p := tk.progress(); p.State != "error" {
		t.Errorf("终态 = %q, want error", p.State)
	}
	tk.errMu.Lock()
	first := tk.firstErr
	tk.errMu.Unlock()
	if first == nil {
		t.Errorf("重试耗尽必须记入 errors（firstErr）")
	}
	if _, statErr := os.Stat(dest); statErr != nil {
		t.Errorf("耗尽后半成品应保留（终判归 Task 11）: %v", statErr)
	}
}

// 错误分类表逐行——半成品生死（§3.10 表，finalizeCleanup 单元）：
// 用户取消 → 删 dest+.qmeta；stalled → 保留；其它取消（反压等）→ 保留；
// 片重试耗尽 → 保留（见完整性终判）。表驱动覆盖四行生死断言。
func TestFinalizeCleanup(t *testing.T) {
	type row struct {
		name       string
		parentDead bool  // 外部取消令牌是否已触发
		err        error // 传给 finalizeCleanup 的任务错误
		wantGone   bool  // 断言 dest/.qmeta 被删
		wantErrIs  error // 返回值须 errors.Is（nil 跳过）
		wantEqual  bool  // 返回值须与 err 同一（原样保留）
	}
	rows := []row{
		{"用户取消-删", true, context.Canceled, true, ErrUserCancel, false},
		{"stalled-保留", false, fmt.Errorf("看门狗: %w", ErrStalled), false, ErrStalled, false},
		{"其它取消-保留", false, context.Canceled, false, nil, true},
		{"片重试耗尽-保留", false, &TaskError{Kind: KindRetryExhausted, Cause: errShortBody}, false, nil, true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "file.bin")
			if err := os.WriteFile(dest, []byte("半"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dest+".qmeta", []byte("meta"), 0o644); err != nil {
				t.Fatal(err)
			}
			tk := newFastTask(t, dest, "http://x.invalid/f", 1, 100)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			if r.parentDead {
				cancel()
			}
			final := tk.finalizeCleanup(parent, r.err)
			if r.wantErrIs != nil && !errors.Is(final, r.wantErrIs) {
				t.Errorf("返回错误 %v 应 Is %v", final, r.wantErrIs)
			}
			if r.wantEqual && final != r.err {
				t.Errorf("返回错误应原样保留, got %v want %v", final, r.err)
			}
			_, dErr := os.Stat(dest)
			_, mErr := os.Stat(dest + ".qmeta")
			if r.wantGone {
				if !os.IsNotExist(dErr) || !os.IsNotExist(mErr) {
					t.Errorf("用户取消应删除 dest+.qmeta（dest: %v, qmeta: %v）", dErr, mErr)
				}
			} else {
				if dErr != nil || mErr != nil {
					t.Errorf("半成品应保留（dest: %v, qmeta: %v）", dErr, mErr)
				}
			}
		})
	}
}

// Range 不支持（分片请求回 200）→ 删除后转单线程全量（§3.10 表行）：
// 无视 Range 的服务器上，分片尝试得 200 即致命退出，收尾转单线程全量
// （原全套头、不带 Range）成功下完整文件——runA 报成功。
func TestRunARangeUnsupportedFallback(t *testing.T) {
	total := int64(500_000)
	data := testPattern(total)
	var reqs []dlReqInfo
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, dlReqInfo{
			hasRange:   r.Header.Get("Range") != "",
			hasCookie:  r.Header.Get("Cookie") != "",
			hasReferer: r.Header.Get("Referer") != "",
			ua:         r.Header.Get("User-Agent"),
		})
		mu.Unlock()
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK) // 无视 Range，一律 200 全量
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk := newFastTask(t, dest, srv.URL, 1, total) // 单 worker：分片尝试恰一个
	err := tk.runA(context.Background())
	if err != nil {
		t.Fatalf("Range 不支持应转单线程全量成功, got %v", err)
	}
	if p := tk.progress(); p.State != "done" || p.Done != total {
		t.Errorf("progress = %+v, want State=done Done=%d", p, total)
	}
	got, rerr := os.ReadFile(dest)
	if rerr != nil || !bytes.Equal(got, data) {
		t.Errorf("转单线程后文件应完整 (%v)", rerr)
	}
	m, lerr := LoadQMeta(dest + ".qmeta")
	if lerr != nil {
		t.Fatalf(".qmeta 应存在: %v", lerr)
	}
	if m.W[0] != total {
		t.Errorf(".qmeta W[0] = %d, want %d（全满完成凭证）", m.W[0], total)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("请求数 = %d, want 2（1 分片尝试 + 1 单线程全量）", len(reqs))
	}
	if !reqs[0].hasRange {
		t.Errorf("首个请求应是带 Range 的分片尝试")
	}
	if reqs[1].hasRange {
		t.Errorf("单线程全量请求不应带 Range")
	}
}

// 直链 4xx 兜底（§3.10 逐字）：失败原因 4xx 且磁盘零进度 → 按序换两组
// 净头（仅 UA+Referer+Origin → 仅 UA）单线程全量重试。服务器按头分派：
// 带 Cookie 或带 Referer 一律 403，仅 UA 才放行——恰好验证两组净头的
// 顺序与内容。
func TestRunAFallback4xxZeroProgress(t *testing.T) {
	total := int64(500_000) // 单片：分片 403 后磁盘必零进度
	data := testPattern(total)
	var reqs []dlReqInfo
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, dlReqInfo{
			hasRange:   r.Header.Get("Range") != "",
			hasCookie:  r.Header.Get("Cookie") != "",
			hasReferer: r.Header.Get("Referer") != "",
			ua:         r.Header.Get("User-Agent"),
		})
		mu.Unlock()
		if r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
			w.WriteHeader(http.StatusForbidden) // 全套头/组1 净头都被拒
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK) // 仅 UA：放行
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk := newFastTask(t, dest, srv.URL, 1, total)
	tk.CookieStr = "foo=bar" // 全套下载头带 Cookie（§3.4），分片请求才有 Cookie 可判
	if err := tk.runA(context.Background()); err != nil {
		t.Fatalf("4xx 零进度应经两组净头兜底成功, got %v", err)
	}
	if p := tk.progress(); p.State != "done" {
		t.Errorf("终态 = %q, want done", p.State)
	}
	got, rerr := os.ReadFile(dest)
	if rerr != nil || !bytes.Equal(got, data) {
		t.Errorf("兜底后文件应完整 (%v)", rerr)
	}

	mu.Lock()
	defer mu.Unlock()
	// 头序逐字：分片(全套头,403) → 组1(UA+Referer+Origin, 无 Cookie,403) → 组2(仅 UA,200)
	if len(reqs) != 3 {
		t.Fatalf("请求数 = %d, want 3（分片 403 + 组1 403 + 组2 成功）: %+v", len(reqs), reqs)
	}
	if !reqs[0].hasRange || !reqs[0].hasCookie {
		t.Errorf("首个请求应是带全套头的分片尝试: %+v", reqs[0])
	}
	if reqs[1].hasRange || reqs[1].hasCookie || !reqs[1].hasReferer || reqs[1].ua == "" {
		t.Errorf("组1 净头应为 仅 UA+Referer+Origin（无 Range/Cookie）: %+v", reqs[1])
	}
	if reqs[2].hasRange || reqs[2].hasCookie || reqs[2].hasReferer || reqs[2].ua == "" {
		t.Errorf("组2 净头应为 仅 UA: %+v", reqs[2])
	}
}

// 4xx 有进度则绝不走兜底（§3.10 逐字）——「兜底会清空半成品，不能毁掉
// 用户几小时的进度」。片 0-2 正常、片 3 恒 403：任务失败但断点全留
// （dest/.qmeta 保留、W 记账原样），且不再发出任何无 Range 的兜底请求。
func TestRunAFallback4xxWithProgress(t *testing.T) {
	total := int64(2 << 20) // 4 片 × 512KB
	data := testPattern(total)
	rec := &rangeRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			t.Errorf("畸形 Range 头: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rec.add(a, b)
		if a >= 3*(512<<10) { // 片 3：恒 403（防盗链类确定性拒绝）
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, total))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[a : b+1])
	}))
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk := newFastTask(t, dest, srv.URL, 1, total) // 单 worker：片序 0→3
	err := tk.runA(context.Background())
	if err == nil {
		t.Fatalf("片 3 恒 403 任务应失败")
	}
	if !causeIs4xx(err) {
		t.Errorf("失败原因应属 4xx, got %v", err)
	}
	if p := tk.progress(); p.State != "error" {
		t.Errorf("终态 = %q, want error", p.State)
	}
	// 断点保留：dest 存在（预分配满尺寸），前 3 片数据在、片 3 区域全零
	got, rerr := os.ReadFile(dest)
	if rerr != nil || int64(len(got)) != total {
		t.Fatalf("半成品 dest 应保留: (%v)", rerr)
	}
	if !bytes.Equal(got[:3*(512<<10)], data[:3*(512<<10)]) {
		t.Errorf("已下 3 片数据应保留在 dest")
	}
	if !bytes.Equal(got[3*(512<<10):], make([]byte, 512<<10)) {
		t.Errorf("片 3 区域应保持全零（403 未写任何字节）")
	}
	m, lerr := LoadQMeta(dest + ".qmeta")
	if lerr != nil {
		t.Fatalf(".qmeta 应保留断点: %v", lerr)
	}
	for i, s := range tk.Segs {
		want := int64(0)
		if i < 3 {
			want = s.End - s.Start + 1
		}
		if m.W[i] != want {
			t.Errorf("W[%d] = %d, want %d（断点记账原样保留）", i, m.W[i], want)
		}
	}
	// 绝不兜底：全部请求都带 Range（无单线程全量请求），恰 4 次
	reqs := rec.snapshot()
	if len(reqs) != 4 {
		t.Fatalf("请求数 = %d, want 4（每片一，403 不重试不兜底）", len(reqs))
	}
}

// 抢占不计失败（§3.10 重试等待期可被抢占；顺序判定框架「先判抢占再判
// 取消」）：注入片级令牌取消而全局未取消 → 片重新入队、不计失败
// （firstErr 空）、不清半成品，任务照常完成。请求期抢占：RoundTripper
// 在片 0 首次请求拨号时掐片级令牌（请求不经真服务器，Range 由注入
// RoundTripper 记录与应答）。
func TestRunAPreemptInFlight(t *testing.T) {
	total := int64(2 << 20) // 4 片
	data := testPattern(total)
	rec := &rangeRecorder{}
	var seg0 atomic.Int32
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk := newFastTask(t, dest, "http://preempt.invalid/x", 1, total)
	tk.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			return nil, err
		}
		rec.add(a, b)
		if a == 0 && seg0.Add(1) == 1 { // 片 0 首次：掐片级令牌（全局不动）
			tk.cancelSeg(0)
			return nil, context.Canceled
		}
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Status:     "206 Partial Content",
			Header:     http.Header{"Content-Range": {fmt.Sprintf("bytes %d-%d/%d", a, b, total)}},
			Body:       io.NopCloser(io.LimitReader(bytes.NewReader(data[a:b+1]), b-a+1)),
		}, nil
	})}
	sr := &sleepRecorder{}
	tk.sleep = sr.sleep

	if err := tk.runA(context.Background()); err != nil {
		t.Fatalf("抢占应消化为重入队并最终成功, got %v", err)
	}
	if p := tk.progress(); p.State != "done" {
		t.Errorf("终态 = %q, want done", p.State)
	}
	tk.errMu.Lock()
	first := tk.firstErr
	tk.errMu.Unlock()
	if first != nil {
		t.Errorf("抢占不计失败, firstErr = %v", first)
	}
	got, rerr := os.ReadFile(dest)
	if rerr != nil || !bytes.Equal(got, data) {
		t.Errorf("抢占重入队后文件应完整 (%v)", rerr)
	}
	// 片 0 被请求两次（首次被抢占 + 重入队再下）
	n0 := 0
	for _, rq := range rec.snapshot() {
		if rq[0] == 0 {
			n0++
		}
	}
	if n0 != 2 {
		t.Errorf("片 0 请求数 = %d, want 2（抢占 → 重新入队）", n0)
	}
	if n := sr.count(); n != 0 {
		t.Errorf("抢占不退避, 退避次数 = %d, want 0", n)
	}
}

// 重试等待期抢占（§3.10 逐字：等待期可被抢占/取消，先判抢占再判取消）：
// 片 0 首次短体 → 进入退避等待（注入桩阻塞）→ 掐片级令牌 → 等待中断、
// 片重新入队、不退避不记失败；再次领取该片从断点 S+W 续传成功。
func TestRunAPreemptDuringBackoff(t *testing.T) {
	total := int64(2 << 20) // 4 片
	data := testPattern(total)
	rec := &rangeRecorder{}
	var seg0 atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			t.Errorf("畸形 Range 头: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rec.add(a, b)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, total))
		w.WriteHeader(http.StatusPartialContent)
		if a == 0 && seg0.Add(1) == 1 { // 片 0 首次：短体诱发退避
			half := (b - a + 1) / 2
			w.Write(data[a : a+half+1])
			return
		}
		w.Write(data[a : b+1])
	}))
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "file.bin")

	tk := newFastTask(t, dest, srv.URL, 1, total)
	sr := &sleepRecorder{block: true, entered: make(chan struct{}, 1)}
	tk.sleep = sr.sleep

	runErr := make(chan error, 1)
	go func() { runErr <- tk.runA(context.Background()) }()

	// 等退避等待真正进入（片级令牌此时必然已登记），再掐它——等待期抢占
	select {
	case <-sr.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("片 0 首次失败后未进入退避等待")
	}
	tk.cancelSeg(0)

	if err := <-runErr; err != nil {
		t.Fatalf("等待期抢占应消化为重入队并最终成功, got %v", err)
	}
	tk.errMu.Lock()
	first := tk.firstErr
	tk.errMu.Unlock()
	if first != nil {
		t.Errorf("抢占不计失败, firstErr = %v", first)
	}
	got, rerr := os.ReadFile(dest)
	if rerr != nil || !bytes.Equal(got, data) {
		t.Errorf("最终文件应完整 (%v)", rerr)
	}
	// 断点续证据：片 0 第二个请求起点 = S + 首次已写（不重复下载）
	half := (tk.Segs[0].End - tk.Segs[0].Start + 1) / 2
	var seg0Reqs [][2]int64
	for _, rq := range rec.snapshot() {
		if rq[0] >= tk.Segs[0].Start && rq[1] <= tk.Segs[0].End {
			seg0Reqs = append(seg0Reqs, rq)
		}
	}
	if len(seg0Reqs) != 2 {
		t.Fatalf("片 0 请求数 = %d, want 2: %v", len(seg0Reqs), seg0Reqs)
	}
	if seg0Reqs[1][0] != tk.Segs[0].Start+half+1 {
		t.Errorf("重入队后续传起点 = %d, want %d（S+W）", seg0Reqs[1][0], tk.Segs[0].Start+half+1)
	}
	if n := sr.count(); n != 1 {
		t.Errorf("等待被抢占即断, 退避记录 = %d, want 1", n)
	}
}
