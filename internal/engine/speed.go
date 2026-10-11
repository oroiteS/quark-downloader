// 速度估计（§3.12）：EWMA 平滑的速度计。监视线程（Task 7）以 200ms
// 节拍喂入累计完成字节数，instant=Δdone/Δt，α=1−exp(−Δt/τ)，τ=1.5s——
// 阶跃后约 τ 秒追上 63%、3τ 追上 95%，既不跟手抖也不迟钝到失真。
package engine

import (
	"math"
	"sync"
	"time"
)

// speedTau 是 EWMA 时间常数 τ（§3.12）：63%/95% 响应点的物理刻度。
const speedTau = 1500 * time.Millisecond

// Speedometer 是 EWMA 速度计（§3.12）。Sample 的节拍由调用方驱动
// （本类型不自己计时，时间全注入，纯逻辑可测）；BPS 供进度事件（Task 14）
// 读取，内部加锁，两头不必同一线程。
type Speedometer struct {
	mu       sync.Mutex
	ewma     float64   // 当前平滑速度（字节/秒）
	lastDone int64     // 上次采样点的累计完成字节
	lastAt   time.Time // 上次采样点时刻
	primed   bool      // 已建立首个基线点
	seeded   bool      // 首个读数已落位
}

// NewSpeedometer 返回尚未采样的速度计：BPS 为 0，等待首个采样建立基线。
func NewSpeedometer() *Speedometer { return &Speedometer{} }

// Sample 喂入一个采样点：done 为累计已完成字节数，now 为当前时刻。
//
//	首个采样只建立基线（尚无 Δ 可言，速度保持 0）；之后每个点
//	instant = Δdone/Δt，α = 1 − exp(−Δt/τ)，ewma = α·instant+(1−α)·ewma。
//	首个读数直接落位（ewma = instant），不从 0 爬坡——否则起步速度会被
//	低估一个 α 因子，瞬时提速显示要等几个 τ 才追平。
//	Δt ≤ 0（同刻采样 / 时钟回拨）的采样点整体跳过、基线不动：既防除零
//	与 NaN，也让下一个正常采样拿到完整的 Δt。
func (s *Speedometer) Sample(done int64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.primed {
		s.lastDone, s.lastAt, s.primed = done, now, true
		return
	}
	dt := now.Sub(s.lastAt)
	if dt <= 0 {
		return
	}
	instant := float64(done-s.lastDone) / dt.Seconds()
	if !s.seeded {
		s.ewma, s.seeded = instant, true // 首读直接落位（§3.12）
	} else {
		alpha := 1 - math.Exp(-dt.Seconds()/speedTau.Seconds())
		s.ewma = alpha*instant + (1-alpha)*s.ewma
	}
	s.lastDone, s.lastAt = done, now
}

// BPS 返回当前 EWMA 速度估计（字节/秒）；尚无读数时为 0。
func (s *Speedometer) BPS() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ewma
}
