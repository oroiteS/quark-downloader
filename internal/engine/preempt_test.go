package engine

import (
	"bytes"
	"context"
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

// 慢连接抢占（§3.7）测试：判定矩阵全部走纯逻辑注入时钟（手动 begin/加 W/
// Scan(注入时刻)，不经网络）；末尾一条 httptest 集成——按连接限速（首条
// 慢 10KB/s、其余快）→ 收尾不被慢连接拖死。时间类常数（扫描周期/年龄
// 门槛/冷却）在 Preemptor 上注入压缩，生产 5s/15s/10s 进测试会拖到分钟级。

// ---- 纯逻辑环境 ---------------------------------------------------------------

// ptEnv 判定矩阵环境：手造 Task（不走网络）+ Preemptor + 毫秒偏移注入时钟。
// base 取 60s：lastPreemptMs 零值（从未被抢）距 base ≥ 冷却上限，不误拦。
type ptEnv struct {
	tk   *Task
	p    *Preemptor
	base time.Time

	mu    sync.Mutex
	fired map[int]bool // 片级令牌被 cancel 的记录（arm 注册的探针）
}

const ptSegSize = int64(2 << 20) // 2MB：快样本每轮 +500KB 也不触剩余护栏

func newPtEnv(workers, segs int) *ptEnv {
	e := &ptEnv{
		base:  time.UnixMilli(60_000),
		fired: make(map[int]bool),
	}
	tk := &Task{
		Segs: make([]Segment, segs),
		W:    make([]int64, segs),
	}
	for i := range tk.Segs {
		tk.Segs[i] = Segment{Index: i, Start: int64(i) * ptSegSize, End: int64(i+1)*ptSegSize - 1}
	}
	e.tk = tk
	e.p = newPreemptor(tk, workers, segs)
	tk.preempt = e.p
	return e
}

// arm 在 segCancel 注册表装 cancel 探针（cancelSeg 的观察点）。
func (e *ptEnv) arm(segs ...int) {
	e.tk.segMu.Lock()
	if e.tk.segCancel == nil {
		e.tk.segCancel = make(map[int]context.CancelFunc)
	}
	for _, idx := range segs {
		e.tk.segCancel[idx] = func() {
			e.mu.Lock()
			e.fired[idx] = true
			e.mu.Unlock()
		}
	}
	e.tk.segMu.Unlock()
}

// firedSet 返回已触发的片集合快照。
func (e *ptEnv) firedSet() map[int]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[int]bool, len(e.fired))
	for k, v := range e.fired {
		out[k] = v
	}
	return out
}

func (e *ptEnv) at(offMs int64) time.Time { return e.base.Add(time.Duration(offMs) * time.Millisecond) }

// begin/end/addW/scan：worker 生命周期钩子与采样输入的注入包装。
func (e *ptEnv) begin(w, idx int, offMs int64) { e.p.begin(w, idx, e.at(offMs)) }
func (e *ptEnv) end(w int)                     { e.p.end(w) }
func (e *ptEnv) addW(idx int, n int64)         { e.tk.W[idx] += n } // 单线程测试直写
func (e *ptEnv) scan(offMs int64)              { e.p.Scan(e.at(offMs)) }

// wantFired 断言 fired 集合恰为 want。
func wantFired(t *testing.T, e *ptEnv, want map[int]bool) {
	t.Helper()
	got := e.firedSet()
	if len(got) != len(want) {
		t.Fatalf("被抢片集合 = %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("被抢片集合 = %v, want %v（缺片 %d）", got, want, k)
		}
	}
}

// 速度记号：样本按「每 5000ms 扫描间隔的增量」换算——
// 慢 5KB/s = +25KB/轮，快 100KB/s = +500KB/轮，20KB/s = +100KB/轮。
const (
	ptSlow  = 25 << 10  // 5KB/s
	ptFast  = 500 << 10 // 100KB/s
	ptMid   = 100 << 10 // 20KB/s
	ptRound = 5_000     // 扫描间隔（ms）
	ptAge   = 15_000    // 常规年龄门槛（ms）
)

// beginFast4 标准开局：4 worker 各领一片 @0、arm 全部片、首轮 Scan 重置基线。
func beginFast4(t *testing.T, e *ptEnv) {
	t.Helper()
	e.arm(0, 1, 2, 3, 4, 5, 6, 7)
	for w := 0; w < 4; w++ {
		e.begin(w, w, 0)
	}
	e.scan(ptRound)
}

// ---- 第一遍：采样 --------------------------------------------------------------

// 换片重置基线（§3.7 逐字「换了片 → 重置基线，本轮无样本」）：worker 从片 0
// 换到片 4（断点 700KB 续传场景），首轮必须以 W[4] 为基线而非沿用片 0 的
// lastW——错位会算出荒谬瞬时速度；且领片后年龄不足期间不产样本不误抢。
// 4 路在飞保持非收尾（占位 w1-3 持续快样本），sampled<4 也不动作。
func TestPreemptBaselineResetOnSegChange(t *testing.T) {
	e := newPtEnv(4, 8)
	e.arm(0, 1, 2, 3, 4)
	e.begin(0, 0, 0)
	for w := 1; w < 4; w++ {
		e.begin(w, w, 0)
	}
	e.scan(ptRound) // 全体重置基线

	e.addW(0, 50<<10) // 片 0 再写（此后换片，这些字节的增量必须被丢弃）
	e.end(0)
	e.addW(4, 700<<10) // 片 4 重新入队后被领走（断点续）
	e.begin(0, 4, 11_000)
	e.scan(15_000) // w0 换片重置基线无样本；w1-3 年龄 15s 出快样本 → sampled=3 <4 不动作
	wantFired(t, e, map[int]bool{})

	e.addW(4, 10<<10) // 2KB/s 慢速
	for w := 1; w < 4; w++ {
		e.addW(w, ptFast)
	}
	e.scan(20_000) // w0 年龄 9s < 15s：无样本 → 慢速也不抢
	wantFired(t, e, map[int]bool{})

	// 基线以片 4 为准：lastW = 710KB（若沿用片 0 的 50KB 会算出 132KB/s 荒谬速度）
	e.p.mu.Lock()
	st := e.p.states[0]
	e.p.mu.Unlock()
	if st.lastIdx != 4 || st.lastW != 710<<10 {
		t.Fatalf("换片后基线 = (lastIdx=%d, lastW=%d), want (4, %d)", st.lastIdx, st.lastW, 710<<10)
	}
}

// 常规年龄门槛 15s（§3.7：新连接还在 TCP 慢启动，速度天然≈0，不排除会把
// 爬坡期全体误判成整体限速）：4 路中 1 慢 3 快，10s 时全无样本（不抢），
// 15s 后出样本 → 慢路被抢。
func TestPreemptAgeGateNormal(t *testing.T) {
	e := newPtEnv(4, 8)
	beginFast4(t, e)

	e.addW(0, ptSlow)
	for w := 1; w < 4; w++ {
		e.addW(w, ptFast)
	}
	e.scan(10_000) // 年龄 10s < 15s：全体无样本 → 不动作
	wantFired(t, e, map[int]bool{})

	e.addW(0, ptSlow)
	for w := 1; w < 4; w++ {
		e.addW(w, ptFast)
	}
	e.scan(ptAge) // 年龄 15s：样本齐 → threshold=均值/2 → 慢路被抢
	wantFired(t, e, map[int]bool{0: true})
}

// 非收尾 sampled<4 不动作（§3.7：样本太少结论不可靠）：4 路在飞但只有
// 1 路年龄达标出样本 → 不抢。
func TestPreemptMinSampled(t *testing.T) {
	e := newPtEnv(4, 8)
	e.arm(0, 1, 2, 3)
	e.begin(0, 0, 0)
	e.scan(ptRound)
	for w := 1; w < 4; w++ {
		e.begin(w, w, 13_000) // 晚领片：本窗口内年龄恒 < 15s
	}
	e.addW(0, 50<<10) // 5KB/s 慢
	e.scan(ptAge)     // w0 年龄 15s 出样本；w1-3 年龄 2s 无样本 → sampled=1 < 4
	wantFired(t, e, map[int]bool{})
}

// threshold 必须用「本轮实测样本」（§3.7 踩坑注释逐字：EWMA 带滞后、在飞
// 数是瞬时值，两者错位会算出荒谬阈值——实测把 6 路全部误判慢、抢占彻底
// 失效）：历史轮全体 20KB/s，本轮 3 快 100KB/s + 1 慢 20KB/s → 本轮均值
// 80KB/s → threshold=40KB/s → 20KB/s 仍被抢。若误用跨轮累计/EWMA（历史
// 均值 20KB/s → threshold 落到 12KB/s 地板），20KB/s 不算慢 → 不抢 → 红。
func TestPreemptThresholdUsesCurrentRoundSamples(t *testing.T) {
	e := newPtEnv(4, 8)
	beginFast4(t, e)

	// 历史轮：全体 20KB/s（两轮把年龄喂过 15s 门槛）
	for w := 0; w < 4; w++ {
		e.addW(w, ptMid)
	}
	e.scan(10_000) // 年龄不足，无样本（但 lastW/lastTick 推进）
	for w := 0; w < 4; w++ {
		e.addW(w, ptMid)
	}
	e.scan(ptAge) // 首个样本轮：全体 20KB/s → threshold=12KB/s 地板 → 无一慢 → 不抢
	wantFired(t, e, map[int]bool{})

	// 本轮：3 快 + 1 慢（20KB/s）→ 本轮均值 80 → threshold 40 → 抢慢路
	e.addW(0, ptMid)
	for w := 1; w < 4; w++ {
		e.addW(w, ptFast)
	}
	e.scan(20_000)
	wantFired(t, e, map[int]bool{0: true})
}

// threshold 地板 12KB/s + 整体限速判定（§3.7：全体都慢 = 服务端配额低，
// 不是个别坏节点；抢占只会造成重连风暴）：4 路全 5KB/s → 均值/2=2.5 →
// 地板 12 → 全部算慢 → slowCount×2 ≥ sampled → 本轮不抢。对照：2 慢 2 快
// 同样 2×2≥4 不抢。
func TestPreemptOverallThrottle(t *testing.T) {
	e := newPtEnv(4, 8)
	beginFast4(t, e)
	for w := 0; w < 4; w++ {
		e.addW(w, ptSlow) // 全体 5KB/s
	}
	e.scan(10_000)
	for w := 0; w < 4; w++ {
		e.addW(w, ptSlow)
	}
	e.scan(ptAge)
	wantFired(t, e, map[int]bool{}) // 全慢 → 整体限速 → 不抢

	// 对照：2 慢 2 快（恰半数慢）→ slowCount×2 = sampled → 仍不抢
	e2 := newPtEnv(4, 8)
	beginFast4(t, e2)
	for round := 0; round < 2; round++ {
		for w := 0; w < 4; w++ {
			if w < 2 {
				e2.addW(w, ptSlow)
			} else {
				e2.addW(w, ptFast)
			}
		}
		if round == 0 {
			e2.scan(10_000)
		} else {
			e2.scan(ptAge)
		}
	}
	wantFired(t, e2, map[int]bool{})
}

// ---- 第二遍：执行条件 ----------------------------------------------------------

// 每轮最多 2 路（§3.7）：7 路在飞 3 慢 4 快（3×2=6 < 7，不触发整体限速）
// → 首轮恰抢 2 路（worker 序）；慢路被消化换片后下一轮补抢第 3 路。
func TestPreemptPerRoundCap(t *testing.T) {
	e := newPtEnv(7, 8)
	e.arm(0, 1, 2, 3, 4, 5, 6)
	for w := 0; w < 7; w++ {
		e.begin(w, w, 0)
	}
	e.scan(ptRound)
	for w := 0; w < 7; w++ {
		if w < 3 {
			e.addW(w, ptSlow)
		} else {
			e.addW(w, ptFast)
		}
	}
	e.scan(10_000)
	for w := 0; w < 7; w++ {
		if w < 3 {
			e.addW(w, ptSlow)
		} else {
			e.addW(w, ptFast)
		}
	}
	e.scan(ptAge) // 3 慢命中但每轮 ≤2：恰 w0/w1
	wantFired(t, e, map[int]bool{0: true, 1: true})

	// 消化抢占：w0/w1 释放换片（空闲），第 3 慢路下轮被抢
	e.end(0)
	e.end(1)
	e.addW(2, ptSlow)
	for w := 3; w < 7; w++ {
		e.addW(w, ptFast)
	}
	e.scan(20_000) // 在飞 5 路仍非收尾；本轮样本 (5+400)/5=81 → threshold 40.5 → w2 慢
	wantFired(t, e, map[int]bool{0: true, 1: true, 2: true})
}

// 非收尾执行条件「该片剩余 ≥ 128KB」（§3.7：快下完的片不值得重连）：
// 慢路只剩 39KB → 不抢；收尾模式跳过该判定 → 照抢。
func TestPreemptRemainGuard(t *testing.T) {
	// 非收尾：4 路在飞，慢路剩余 64KB−25KB=39KB < 128KB
	e := newPtEnv(4, 8)
	beginFast4(t, e)
	e.addW(0, ptSegSize-64<<10) // 预置到剩余 64KB（beginFast4 的基线扫描在预置前）
	// 基线已含旧 W：重新 begin 同一片强制下轮重置基线（领片后的存量不算增量）
	e.end(0)
	e.begin(0, 0, 1)
	e.scan(ptRound + 1) // 重置基线：lastW = W[0]（剩余 64KB）
	e.addW(0, ptSlow)
	for w := 1; w < 4; w++ {
		e.addW(w, ptFast)
	}
	e.scan(ptAge + 1) // 年龄 ~15s、慢样本命中但剩余 39KB < 128KB → 不抢
	wantFired(t, e, map[int]bool{})

	// 收尾：3 路在飞、慢路剩余 39KB → 跳过剩余判定 → 抢
	e2 := newPtEnv(3, 8)
	e2.arm(0, 1, 2, 3)
	for w := 0; w < 3; w++ {
		e2.begin(w, w, 0)
	}
	e2.addW(0, ptSegSize-64<<10) // 领片前的存量（剩余 64KB），基线含它
	e2.scan(3_000)               // 收尾门槛 3s：重置基线（w0 基线 = 含存量的 W[0]）
	e2.addW(0, ptSlow)           // +25KB → 剩余 39KB
	e2.addW(1, ptFast)
	e2.addW(2, ptFast)
	e2.scan(6_000) // 收尾（在飞 3 路）：门槛 3s、样本齐、剩余判定跳过 → 抢
	wantFired(t, e2, map[int]bool{0: true})
}

// 片被抢次数上限（常规 3）：count=3 → 不抢；count=2 → 抢（递增到 3）。
func TestPreemptCountCap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		want  bool
	}{
		{"count3-不抢", 3, false},
		{"count2-可抢", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPtEnv(4, 8)
			beginFast4(t, e)
			e.p.mu.Lock()
			e.p.preemptCount[0] = tc.count
			e.p.mu.Unlock()
			e.addW(0, ptSlow)
			for w := 1; w < 4; w++ {
				e.addW(w, ptFast)
			}
			e.scan(10_000)
			e.addW(0, ptSlow)
			for w := 1; w < 4; w++ {
				e.addW(w, ptFast)
			}
			e.scan(ptAge)
			want := map[int]bool{}
			if tc.want {
				want[0] = true
			}
			wantFired(t, e, want)
			if tc.want {
				e.p.mu.Lock()
				got := e.p.preemptCount[0]
				e.p.mu.Unlock()
				if got != 3 {
					t.Errorf("抢占后 count = %d, want 3（2 → 3）", got)
				}
			}
		})
	}
}

// 常规冷却 10s（§3.7：距该片上次被抢 ≥ 冷却）：5s 前被抢 → 不抢；
// 12s 前被抢 → 抢。
func TestPreemptCooldown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agoMs int64
		want  bool
	}{
		{"5s前-冷却中不抢", 5_000, false},
		{"12s前-冷却过可抢", 12_000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPtEnv(4, 8)
			beginFast4(t, e)
			e.p.mu.Lock()
			e.p.lastPreemptMs[0] = e.at(ptAge - tc.agoMs).UnixMilli()
			e.p.mu.Unlock()
			e.addW(0, ptSlow)
			for w := 1; w < 4; w++ {
				e.addW(w, ptFast)
			}
			e.scan(10_000)
			e.addW(0, ptSlow)
			for w := 1; w < 4; w++ {
				e.addW(w, ptFast)
			}
			e.scan(ptAge)
			want := map[int]bool{}
			if tc.want {
				want[0] = true
			}
			wantFired(t, e, want)
		})
	}
}

// ---- 收尾放宽（在飞 ≤ 3 路）----------------------------------------------------

// 收尾跳过两条判定（sampled<4 / 整体限速）且门槛降到 3s：4 个 worker 中
// 仅 3 个在飞（空闲 worker 不计在飞）→ 收尾；3 路全慢 5KB/s（非收尾会判
// 整体限速）→ 仍抢，每轮 ≤2 路。
func TestPreemptTailSkipsGuards(t *testing.T) {
	e := newPtEnv(4, 8)
	e.arm(0, 1, 2, 3)
	for w := 0; w < 3; w++ {
		e.begin(w, w, 0) // w3 空闲：不计在飞
	}
	e.scan(3_000) // 收尾门槛 3s：首轮重置基线
	for w := 0; w < 3; w++ {
		e.addW(w, ptSlow)
	}
	e.scan(6_000) // 年龄 6s ≥ 3s：3 路全慢（slowCount×2 ≥ sampled）但收尾跳过 → 抢前 2 路
	wantFired(t, e, map[int]bool{0: true, 1: true})

	e.end(0)
	e.end(1)
	e.addW(2, ptSlow)
	e.scan(9_000) // 剩 1 路仍收尾 → 补抢第 3 路
	wantFired(t, e, map[int]bool{0: true, 1: true, 2: true})
}

// 收尾放宽上限与冷却（§3.7 依据：常规 3 次在收尾 30s 内烧完，剩下的片
// 只能干等到停滞看门狗中止）：count=3（常规上限已满）收尾仍可抢（<30）；
// 冷却 3s——2s 前被抢不抢、4s 前可抢。
func TestPreemptTailLooseLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agoMs int64
		want  bool
	}{
		{"冷却2s-不抢", 2_000, false},
		{"冷却4s-可抢", 4_000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPtEnv(1, 4)
			e.arm(0)
			e.begin(0, 0, 0)
			e.scan(3_000)
			e.p.mu.Lock()
			e.p.preemptCount[0] = 3 // 常规上限已满：收尾放宽到 30
			e.p.lastPreemptMs[0] = e.at(10_000 - tc.agoMs).UnixMilli()
			e.p.mu.Unlock()
			e.addW(0, ptSlow)
			e.scan(10_000) // 单路在飞 = 收尾：年龄 10s ≥ 3s、count 3 < 30、冷却按 3s 判
			want := map[int]bool{}
			if tc.want {
				want[0] = true
			}
			wantFired(t, e, want)
		})
	}
}

// ---- 集成：收尾不被慢连接拖死 ---------------------------------------------------

// httptest 按连接限速（一条 3KB/s 量级慢、其余快）→ 收尾不拖死（§3.7 要治
// 的病）：片 0 首请求以 ~10KB/s（低于 12KB/s 地板）慢爬，其余片与片 0 的
// 断点续请求全速。快 worker 干完 7 片后在飞只剩慢路 → 收尾模式（门槛 3s、
// 跳过样本数/整体限速/剩余判定）→ 慢路被掐 → 片 0 重入队 → 等待中的快
// worker 从断点 S+W 续传全速收尾。周期全注入压缩（扫描 20ms、门槛 60ms、
// 冷却 30ms）。
func TestRunAPreemptSlowTail(t *testing.T) {
	total := int64(4 << 20) // 4MB → 8 片 × 512KB
	const threads = 4
	data := testPattern(total)
	dest := filepath.Join(t.TempDir(), "file.bin")

	rec := &rangeRecorder{}
	var slowHits atomic.Int32
	var slowSent atomic.Int64 // 慢爬轮实际发出的字节数（断点续起点对照）
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
		if a == 0 && slowHits.Add(1) == 1 {
			// 慢连接：100B/10ms ≈ 10KB/s（< 12KB/s 地板），片级令牌掐断即中断
			f, _ := w.(http.Flusher)
			for off := a; off <= b; {
				select {
				case <-r.Context().Done():
					return // 被抢占：连接掐断（fetchOnce 报错 → errPreempted → 重入队）
				default:
				}
				end := min(off+100, b+1)
				if _, err := w.Write(data[off:end]); err != nil {
					return
				}
				if f != nil {
					f.Flush() // 立即下发：客户端 W 才增长、监视线程才有样本
				}
				slowSent.Add(end - off)
				off = end
				time.Sleep(10 * time.Millisecond)
			}
			return
		}
		w.Write(data[a : b+1]) // 其余片与断点续：全速
	}))
	t.Cleanup(srv.Close)

	tk := newFastTask(t, dest, srv.URL, threads, total)
	tk.preemptEvery = 20 * time.Millisecond // 扫描周期注入（生产 5s）
	p := tk.preempt
	p.ageGate = 120 * time.Millisecond     // 常规门槛（生产 15s）
	p.ageGateTail = 60 * time.Millisecond  // 收尾门槛（生产 3s）
	p.cooldown = 100 * time.Millisecond    // 常规冷却（生产 10s）
	p.cooldownTail = 30 * time.Millisecond // 收尾冷却（生产 3s）

	start := time.Now()
	if err := tk.runA(context.Background()); err != nil {
		t.Fatalf("慢连接收尾应经抢占自愈完成, got %v", err)
	}
	if p := tk.progress(); p.State != "done" || p.Done != total {
		t.Errorf("progress = %+v, want State=done Done=%d", p, total)
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

	// 抢占确实发生且重复发生受控：片 0 被抢 ≥1 次
	p.mu.Lock()
	pc := p.preemptCount[0]
	p.mu.Unlock()
	if pc < 1 {
		t.Fatalf("片 0 应被抢占 ≥1 次, got %d（慢连接未被识别或未被掐断）", pc)
	}

	// 片 0 ≥2 个请求；断点续语义：续传起点 ∈ (0, slowSent]——已写字节保留在 W
	// 不重复下载（慢爬发出的字节最多丢在途的一小块）
	var seg0Reqs [][2]int64
	for _, rq := range rec.snapshot() {
		if rq[0] >= tk.Segs[0].Start && rq[1] <= tk.Segs[0].End {
			seg0Reqs = append(seg0Reqs, rq)
		}
	}
	if len(seg0Reqs) < 2 {
		t.Fatalf("片 0 请求数 = %d, want ≥2（慢爬被抢 → 断点续重连）: %v", len(seg0Reqs), seg0Reqs)
	}
	resume := seg0Reqs[1][0]
	sent := slowSent.Load()
	if resume <= 0 || resume > sent {
		t.Errorf("断点续起点 = %d, want ∈ (0, %d]（S+W，不重复下载）", resume, sent)
	}

	// 不被拖死：慢爬 512KB@10KB/s ≈ 52s；抢占生效应秒级完成
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("收尾被慢连接拖死: 总耗时 %v（抢占应秒级收尾）", el)
	}
}
