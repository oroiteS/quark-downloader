package engine

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 限流闸门（§3.8）测试：纯闸门行为（阻塞/收缩/下限/恢复节奏，时钟
// 注入）+ worker 侧接线（429/503 → Shrink、监视线程 → TickRecover）。
// 并发压测须在 -race 下跑（规格⚠️：锁内非阻塞收票的死锁事故注）。

// TestGateAcquireBlocksAtLimit 初始 N 路 acquire 成功、第 N+1 阻塞。
func TestGateAcquireBlocksAtLimit(t *testing.T) {
	g := NewGate(3)
	ctx := context.Background()
	for i := range 3 {
		if err := g.Acquire(ctx); err != nil {
			t.Fatalf("第 %d 路 Acquire 应成功: %v", i+1, err)
		}
	}
	// 第 4 路：池空只能等 100ms 超时（阻塞的证明 = DeadlineExceeded 且
	// 真的耗满了超时窗口）。
	ctx4, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := g.Acquire(ctx4); err == nil {
		t.Fatal("第 4 路 Acquire 不应成功（上限 3）")
	} else if !isCancellation(err) {
		t.Fatalf("第 4 路应为 ctx 超时错误, 得到 %v", err)
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Fatalf("第 4 路应阻塞到超时, 实际 %v 即返回", elapsed)
	}
	// Release 一张后立刻可进（票据真的归还了）。
	g.Release()
	if err := g.Acquire(ctx); err != nil {
		t.Fatalf("Release 后 Acquire 应成功: %v", err)
	}
}

// TestGateAcquireErrorKeepsContract 契约（Task 7 锚定）：Acquire 返回
// 错误 ⇒ 调用方不得持有票、不得 Release——取消的等待者不能偷票。
func TestGateAcquireErrorKeepsContract(t *testing.T) {
	g := NewGate(2)
	ctx := context.Background()
	if err := g.Acquire(ctx); err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	if err := g.Acquire(ctx); err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}
	// 等待者被取消：返回错误且不给票。
	wctx, wcancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- g.Acquire(wctx) }()
	time.Sleep(30 * time.Millisecond) // 让等待者先挂上
	wcancel()
	if err := <-done; err == nil {
		t.Fatal("被取消的 Acquire 应返回错误")
	}
	// 已取消的 ctx 即使池里有票也不放行（与 noOpGate 语义一致）。
	g.Release()
	if err := g.Acquire(wctx); err == nil {
		t.Fatal("已取消的 ctx 不应拿到票")
	}
	g.Release() // 归还上一段借的票, 保持池满 2
}

// TestGateShrinkCollectsIdleTickets 池中有闲票时 Shrink 直接收差额：
// 10 → 7，此后恰好 7 路 acquire 成功、第 8 阻塞。
func TestGateShrinkCollectsIdleTickets(t *testing.T) {
	g := NewGate(10)
	g.Shrink()
	if g.limit != 7 {
		t.Fatalf("Shrink 后 limit 应 10×0.7=7, 得到 %d", g.limit)
	}
	ctx := context.Background()
	for i := range 7 {
		if err := g.Acquire(ctx); err != nil {
			t.Fatalf("第 %d 路 Acquire 应成功: %v", i+1, err)
		}
	}
	ctx8, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := g.Acquire(ctx8); err == nil {
		t.Fatal("第 8 路 Acquire 不应成功（Shrink 后上限 7）")
	}
}

// TestGateShrinkConvergesViaRelease 票全在飞时 Shrink（规格⚠️ 死锁事故
// 注的核心场景）：非阻塞收票收不满就算了、绝不阻塞；差额由 Release 的
// 条件放回逐步收敛——10 路占满 → Shrink 到 7 → 全部 Release 后恰好
// 7 路可进。
func TestGateShrinkConvergesViaRelease(t *testing.T) {
	g := NewGate(10)
	ctx := context.Background()
	for range 10 {
		if err := g.Acquire(ctx); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
	}
	// 池空 + 限流时票恰被占满：Shrink 若阻塞等待 = 握锁让所有 worker
	// 的 Release 排队 = 死锁（真实踩过）。断言立即返回。
	shrunk := make(chan struct{})
	go func() { g.Shrink(); close(shrunk) }()
	select {
	case <-shrunk:
	case <-time.After(2 * time.Second):
		t.Fatal("Shrink 在票占满时阻塞（§3.8：绝不能阻塞等待收票）")
	}
	if g.limit != 7 {
		t.Fatalf("Shrink 后 limit 应 7, 得到 %d", g.limit)
	}
	for range 10 {
		g.Release()
	}
	for i := range 7 {
		if err := g.Acquire(ctx); err != nil {
			t.Fatalf("收敛后第 %d 路 Acquire 应成功: %v", i+1, err)
		}
	}
	ctx8, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := g.Acquire(ctx8); err == nil {
		t.Fatal("收敛后第 8 路 Acquire 不应成功（稳态上限 7）")
	}
}

// TestGateShrinkFloorFour 连续收缩的最低下限是 4（规格逐字
// gateLimit = max(4, ×0.7)）：64 → 44 → 30 → 21 → 14 → 9 → 6 → 4 → 4。
func TestGateShrinkFloorFour(t *testing.T) {
	g := NewGate(64)
	want := []int{44, 30, 21, 14, 9, 6, 4, 4}
	for i, w := range want {
		g.Shrink()
		if g.limit != w {
			t.Fatalf("第 %d 次 Shrink 后 limit 应 %d, 得到 %d", i+1, w, g.limit)
		}
	}
	ctx := context.Background()
	for i := range 4 {
		if err := g.Acquire(ctx); err != nil {
			t.Fatalf("下限 4 内第 %d 路 Acquire 应成功: %v", i+1, err)
		}
	}
	ctx5, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := g.Acquire(ctx5); err == nil {
		t.Fatal("下限 4 下第 5 路 Acquire 不应成功")
	}
}

// TestGateShrinkNeverExceedsInitial 初始 N<4 的任务：收缩下限 4 不得
// 反向放大闸门（闸门只收紧不放开，恢复终点才是 N）。
func TestGateShrinkNeverExceedsInitial(t *testing.T) {
	g := NewGate(2)
	g.Shrink()
	if g.limit != 2 {
		t.Fatalf("N=2 时 Shrink 不应改变 limit（不得放大到 4）, 得到 %d", g.limit)
	}
}

// TestGateRecoverRhythm 恢复节奏（注入时钟，规格逐字：距上次限流
// ≥30s 且 gateLimit<N → +1）：
//   - 不足 30s 不恢复；
//   - 满 30s +1，且恢复后重新冷却 30s（恢复周期故意长——放太快来回抖
//     更糟，监视线程 200ms 一跳不得连环放票冲回 N）；
//   - 恢复必须存在（只缩不放 = 永远 4 路的事故注释）：推时钟必回 N；
//   - limit==N 后不再放。
func TestGateRecoverRhythm(t *testing.T) {
	g := NewGate(10)
	g.Shrink() // 10 → 7
	if g.limit != 7 {
		t.Fatalf("Shrink 后 limit 应 7, 得到 %d", g.limit)
	}
	base := g.lastAdj

	g.TickRecover(base.Add(29 * time.Second))
	if g.limit != 7 {
		t.Fatalf("距限流 29s 不应恢复, limit=%d", g.limit)
	}
	g.TickRecover(base.Add(30 * time.Second))
	if g.limit != 8 {
		t.Fatalf("距限流 30s 应恢复到 8, limit=%d", g.limit)
	}
	// 恢复后冷却重置：+0.2s（相对上次恢复 30s 差一点）不得再 +1。
	g.TickRecover(base.Add(30*time.Second + 200*time.Millisecond))
	if g.limit != 8 {
		t.Fatalf("恢复后冷却期内不应再 +1, limit=%d", g.limit)
	}
	// 每 30s 一格推到 N=10（恢复必须存在）。
	now := base.Add(30 * time.Second)
	for g.limit < g.n {
		now = now.Add(gateRecoverEvery)
		g.TickRecover(now)
	}
	if g.limit != 10 {
		t.Fatalf("恢复终点应是初始 N=10, limit=%d", g.limit)
	}
	g.TickRecover(now.Add(time.Hour))
	if g.limit != 10 {
		t.Fatalf("limit==N 后不应继续恢复, limit=%d", g.limit)
	}
	// 恢复的每一步都真补了票：现在恰好 10 路可进、第 11 阻塞。
	ctx := context.Background()
	for i := range 10 {
		if err := g.Acquire(ctx); err != nil {
			t.Fatalf("恢复满额后第 %d 路 Acquire 应成功: %v", i+1, err)
		}
	}
	ctx11, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := g.Acquire(ctx11); err == nil {
		t.Fatal("第 11 路 Acquire 不应成功（上限 10）")
	}
}

// TestGateShrinkResetsRecoverClock 收缩刷新恢复冷却锚点：恢复到一半又
// 限流，30s 内不得继续恢复。
func TestGateShrinkResetsRecoverClock(t *testing.T) {
	g := NewGate(10)
	g.Shrink() // 7
	base := g.lastAdj
	g.TickRecover(base.Add(30 * time.Second)) // 8
	g.Shrink()                                // max(4, 8×0.7)=5, 冷却重置
	if g.limit != 5 {
		t.Fatalf("二次 Shrink 后 limit 应 5, 得到 %d", g.limit)
	}
	base2 := g.lastAdj
	g.TickRecover(base2.Add(gateRecoverEvery - time.Second))
	if g.limit != 5 {
		t.Fatalf("新限流后 29s 不应恢复, limit=%d", g.limit)
	}
	g.TickRecover(base2.Add(gateRecoverEvery))
	if g.limit != 6 {
		t.Fatalf("新限流后满 30s 应恢复到 6, limit=%d", g.limit)
	}
}

// TestGateConcurrentStress 并发压测（-race）：N 路 worker 高频
// Acquire/Release（含超时取消路径），捣乱者并发 Shrink / TickRecover。
// 断言不死锁（挂起即测试超时暴露）、瞬时并发不破初始上限、limit 全程
// 合法（≤N，且 ≥ min(4,N)）。
func TestGateConcurrentStress(t *testing.T) {
	const n = 8
	g := NewGate(n)
	stop := make(chan struct{})
	var inFlight, maxFlight, minLimit, maxLimitSeen atomic.Int64
	minLimit.Store(n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// 20ms 超时：池满时等待者常态走取消路径（契约压测）。
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				if err := g.Acquire(ctx); err != nil {
					cancel()
					continue
				}
				cur := inFlight.Add(1)
				for {
					old := maxFlight.Load()
					if cur <= old || maxFlight.CompareAndSwap(old, cur) {
						break
					}
				}
				time.Sleep(50 * time.Microsecond) // 让并发真正重叠
				inFlight.Add(-1)
				g.Release()
				cancel()
			}
		}()
	}
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				g.Shrink()
				g.TickRecover(time.Now().Add(time.Hour)) // 伪时刻疯狂触发恢复判定
				lim := int64(g.Limit())
				for {
					old := minLimit.Load()
					if lim >= old || minLimit.CompareAndSwap(old, lim) {
						break
					}
				}
				for {
					old := maxLimitSeen.Load()
					if lim <= old || maxLimitSeen.CompareAndSwap(old, lim) {
						break
					}
				}
			}
		}()
	}
	time.Sleep(250 * time.Millisecond)
	close(stop)
	wg.Wait()

	if mf := maxFlight.Load(); mf > n {
		t.Fatalf("瞬时并发 %d 破初始上限 %d", mf, n)
	}
	if ml := minLimit.Load(); ml < min(gateMinLimit, n) {
		t.Fatalf("limit 收缩到 %d，破下限 %d", ml, min(gateMinLimit, n))
	}
	if xl := maxLimitSeen.Load(); xl > n {
		t.Fatalf("limit 恢复到 %d，超初始 N=%d", xl, n)
	}
}

// ---- worker 侧接线 ------------------------------------------------------------

// shrinkRecorder 记录 Shrink/TickRecover 调用的假闸门（Acquire/Release
// 直通自 noOpGate）。
type shrinkRecorder struct {
	noOpGate
	shrinks atomic.Int32
	ticks   atomic.Int32
}

func (s *shrinkRecorder) Shrink()               { s.shrinks.Add(1) }
func (s *shrinkRecorder) TickRecover(time.Time) { s.ticks.Add(1) }

// TestFetchSegShrinksGateOnRateLimit §3.8 逐字：收到 429/503 →
// gate.Shrink()。片级重试每次尝试都撞限流响应，每次都应触发收缩
// （1 首试 + 4 重试 = 5 次）。
func TestFetchSegShrinksGateOnRateLimit(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			sg := &shrinkRecorder{}
			tk := &Task{
				URL:   "http://gate.test/file",
				Segs:  []Segment{{Start: 0, End: 1 << 10}},
				W:     []int64{0},
				gate:  sg,
				sleep: func(context.Context, time.Duration) error { return nil },
				client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: status,
						Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
						Body:       http.NoBody,
						Header:     make(http.Header),
						Request:    r,
					}, nil
				})},
			}
			err := tk.fetchSeg(context.Background(), 0, make([]byte, 4<<10), rand.New(rand.NewPCG(1, 2)))
			if err == nil {
				t.Fatal("恒 429/503 应以重试耗尽失败")
			}
			if got, want := sg.shrinks.Load(), int32(segRetryMax+1); got != want {
				t.Fatalf("Shrink 调用 %d 次, 期望每次尝试共 %d 次", got, want)
			}
		})
	}
}

// TestRunAMonitorTicksRecover 监视线程周期调 gate.TickRecover（§3.8
// 恢复节奏钩子，真实时钟由 monitor 的 ticker 传入）。
func TestRunAMonitorTicksRecover(t *testing.T) {
	total := int64(2 << 20)
	data := testPattern(total)
	srv, _ := newRangeServer(t, data)
	dest := filepath.Join(t.TempDir(), "f.bin")
	tk := newFastTask(t, dest, srv.URL, 4, total)
	sg := &shrinkRecorder{}
	tk.gate = sg
	if err := tk.runA(context.Background()); err != nil {
		t.Fatalf("runA: %v", err)
	}
	if sg.ticks.Load() == 0 {
		t.Fatal("监视线程未调用 gate.TickRecover（§3.8 恢复钩子未接线）")
	}
}
