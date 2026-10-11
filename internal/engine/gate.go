// 限流闸门（§3.8「限流制动」）：令牌 channel 实现，零新依赖。
//
//	收到 429/503 → gateLimit = max(4, ×0.7)，非阻塞回收差额票
//	每 30s 检查：距上次限流 ≥30s 且 gateLimit < N → +1
//
// 设计不变量：tokens 容量恒为初始 N；任意时刻池内票数 ≤ 当前 limit。
// Shrink 收不满的差额不追——在飞的票由 Release 的条件放回（池未满
// 当前 limit 才归还）逐步收敛，稳态并发 = limit。
package engine

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// §3.8/§7 常数。
const (
	// gateMinLimit 收缩下限（规格逐字 gateLimit = max(4, ×0.7)）：
	// 并发压得太低会让单文件吞吐崩塌，4 是「再惨也要保住」的地板。
	gateMinLimit = 4
	// gateRecoverEvery 恢复检查节奏（规格逐字每 30s 检查）：距上次
	// 限流 ≥30s 才放 1 票。恢复周期故意长——放太快来回抖更糟（服务器
	// 还在限流边缘，快速放开立刻又 429，闸门打摆）。
	gateRecoverEvery = 30 * time.Second
)

// Gate 是限流闸门（§3.8）：worker 领片前 Acquire、片处理完 Release，
// 429/503 触发 Shrink 收缩，监视线程周期调 TickRecover 恢复。
// 实现 Task.gate 接口（Ruling E1：Acquire 返回错误 ⇒ 调用方不持票、
// 不得 Release；ctx 取消时 Acquire 返回错误让 worker 退出）。
type Gate struct {
	mu      sync.Mutex
	tokens  chan struct{} // 令牌池：容量 = 初始 N；池内票数 ≤ limit（见文件头不变量）
	limit   int           // 当前并发上限 gateLimit（只被 Shrink/TickRecover 改写）
	n       int           // 初始上限 N：恢复的终点，闸门永不放大超过它
	lastAdj time.Time     // 上次闸门动作（收缩或恢复）时刻：恢复冷却的锚点
}

// NewGate 构造初始 limit 路并发的闸门（Task 14 以 N 装配）。
func NewGate(limit int) *Gate {
	if limit < 1 { // 装配防御：runA 已保证 n≥1，这里兜底钳 1
		limit = 1
	}
	g := &Gate{
		tokens: make(chan struct{}, limit),
		limit:  limit,
		n:      limit,
	}
	for range limit {
		g.tokens <- struct{}{} // 池容量 = limit，填满必不阻塞
	}
	return g
}

// 编译期断言：Gate 实现 Task.gate 契约（Ruling E1）。
var _ gate = (*Gate)(nil)

// Acquire 取一张票；池空则阻塞等票或 ctx 结束。拿到票后补查一次
// ctx：已取消的任务即使池里有票也不放行（与 noOpGate 语义一致），
// 此时票就地归还——契约「返回错误 ⇒ 不持票」由本方法自己兜住。
func (g *Gate) Acquire(ctx context.Context) error {
	select {
	case <-g.tokens:
	case <-ctx.Done():
		return ctx.Err() // 未持票，调用方不得 Release
	}
	if err := ctx.Err(); err != nil {
		g.Release()
		return err
	}
	return nil
}

// Release 归还一张票。收缩后已发出但收不回的票在这里自然回收：
// 池内票数已达当前 limit 时这张票直接注销（超额持有的收敛点），
// 稳态并发由此钉在 limit 上。锁内只做非阻塞 channel 操作。
func (g *Gate) Release() {
	g.mu.Lock()
	if len(g.tokens) < g.limit {
		select {
		case g.tokens <- struct{}{}:
		default: // len<limit≤容量 时必成功；default 纯防御，绝不阻塞
		}
	}
	g.mu.Unlock()
}

// Shrink 收到 429/503 时收缩（规格逐字 gateLimit = max(4, ×0.7)）：
// 新上限同时钳在初始 N 内——闸门只收紧不放大（N<4 的小任务不受地板
// 反向抬升）。然后从池中**非阻塞**回收差额票。
//
// ⚠️ 绝不能阻塞等待收票：本操作在锁内执行，而限流发生时票恰被占满
// （池空）——阻塞等待 = 握着 mu 让所有 worker 的 Release 排队 = 死锁
// （§3.8 真实踩过的事故）。收不满就算了：在飞的票由 Release 的条件
// 放回逐步收敛。即使已收缩到底也刷新冷却锚点——限流现场刚发生，
// 30s 内不该放票。
func (g *Gate) Shrink() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastAdj = time.Now()
	next := min(max(gateMinLimit, g.limit*7/10), g.n)
	if next >= g.limit {
		return
	}
	diff := g.limit - next
	g.limit = next
	for range diff {
		select {
		case <-g.tokens: // 非阻塞收一张
		default:
			return // 池空（全在飞）：收不满就算了（见上⚠️）
		}
	}
}

// TickRecover 恢复检查（规格逐字：距上次限流 ≥30s 且 gateLimit<N →
// +1），由监视线程以 200ms 采样节拍周期调用、now 注入（测试时钟）。
// 恢复必须存在——只缩不放 = 一次 429 后永远 4 路（§3.8 事故注释）。
// 锚点在每次恢复后推进（lastAdj = 上次收缩**或**恢复）：否则 200ms
// 一跳的监视线程会在满 30s 后的连续几跳里连环放票、瞬间冲回 N，
// 「恢复周期故意长（放太快来回抖更糟）」就被打穿。
func (g *Gate) TickRecover(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limit >= g.n || now.Sub(g.lastAdj) < gateRecoverEvery {
		return
	}
	g.limit++
	g.lastAdj = now
	select {
	case g.tokens <- struct{}{}: // 放回 1 票：池内 ≤ 旧 limit < 新 limit ≤ 容量，必成功
	default: // 不变量被挤破时的兜底：宁少放一张也不阻塞
	}
}

// Limit 返回当前并发上限（加锁读；诊断/装配与并发测试用）。
func (g *Gate) Limit() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// isRateLimited §3.8 逐字：429/503 是限流信号（其余 5xx 不收缩——
// 服务器故障不等于限流制动场景）。
func isRateLimited(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
}
