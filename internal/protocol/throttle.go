package protocol

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// throttleBase 是节流基准间隔:任意两次 API 请求间隔 ≥ 260ms × (0.75 + 0.5×rand),
// 随机抖动打破固定机器节奏(§2.3)。
const throttleBase = 260 * time.Millisecond

// Throttler 保证相邻两次放行(Wait 返回)的实际间隔落在
// [260ms×0.75, 260ms×1.25],每次等待独立抽取抖动。
type Throttler struct {
	mu   sync.Mutex
	last time.Time // 上次预定(且实际发生)的放行时刻;零值表示尚未放行过
}

// NewThrottler 创建节流器;首次 Wait 立即放行。
func NewThrottler() *Throttler {
	return &Throttler{}
}

// Wait 阻塞到与上次放行间隔满一个随机节流周期后放行。
// 放行时刻在锁内一次性预定并推进:last → last+interval,随后在锁外
// 睡到预定时刻再返回——并发调用者由此按预定顺序依次放行,任意两次
// 实际放行的间隔恒 ≥ 260ms×0.75(旧实现锁外重读 last 会让并发调用
// 各自独立抽取 interval 后抢先放行,间隔可趋近 0ms)。
// ctx 先行取消则返回 ctx.Err();已预定的槽位作废、不回退放行时刻,
// 这只会让后续放行更保守(间隔更长),不会破坏下界——若回退,后来者
// 会以更早的时刻为基准预定,反而可能与已发生的放行贴得过近。
func (t *Throttler) Wait(ctx context.Context) error {
	t.mu.Lock()
	var release time.Time // 本次放行的预定时刻
	if t.last.IsZero() {
		release = time.Now() // 首次调用立即放行
	} else {
		interval := time.Duration(float64(throttleBase) * (0.75 + 0.5*rand.Float64()))
		release = t.last.Add(interval)
	}
	t.last = release
	t.mu.Unlock()

	// 预定时刻可能已过(前序放行拖延后排队追赶),此时立即放行。
	if now := time.Now(); now.Before(release) {
		timer := time.NewTimer(release.Sub(now))
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}
