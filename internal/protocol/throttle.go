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
	last time.Time // 上次放行时刻;零值表示尚未放行过
}

// NewThrottler 创建节流器;首次 Wait 立即放行。
func NewThrottler() *Throttler {
	return &Throttler{}
}

// Wait 阻塞到与上次放行间隔满一个随机节流周期后放行;
// ctx 先行取消则返回 ctx.Err(),此次不放行、不推进放行时刻。
func (t *Throttler) Wait(ctx context.Context) error {
	t.mu.Lock()
	last := t.last
	t.mu.Unlock()

	var release time.Time // 本次放行时刻
	if last.IsZero() {
		release = time.Now()
	} else {
		interval := time.Duration(float64(throttleBase) * (0.75 + 0.5*rand.Float64()))
		target := last.Add(interval)
		if now := time.Now(); now.Before(target) {
			timer := time.NewTimer(target.Sub(now))
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
		release = time.Now()
	}

	t.mu.Lock()
	t.last = release
	t.mu.Unlock()
	return nil
}
