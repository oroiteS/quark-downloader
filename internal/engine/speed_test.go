package engine

import (
	"math"
	"testing"
	"time"
)

// §3.12 常量与监视线程（Task 7）的采样节拍。
const (
	spdStep = 200 * time.Millisecond // 采样节拍：由调用方驱动
)

// feedConst 在 [from, to) 内以恒速 bps（字节/秒）按 200ms 节拍喂采样点，
// 返回区间结束时的累计字节数。bps 取 100/400 这类 ×0.2 为整数的值，
// 字节数全程精确、无取整漂移。
func feedConst(s *Speedometer, t0 time.Time, from, to time.Duration, bps float64, done int64) int64 {
	for t := from; t+spdStep <= to; t += spdStep {
		done += int64(bps * spdStep.Seconds())
		s.Sample(done, t0.Add(t+spdStep))
	}
	return done
}

// 未采样与仅建立基线时，BPS 应为 0——首个采样点没有 Δ 可言。
func TestSpeedometerBPSZeroBeforeFirstReading(t *testing.T) {
	s := NewSpeedometer()
	if got := s.BPS(); got != 0 {
		t.Errorf("全新速度计 BPS = %v, 期望 0", got)
	}
	t0 := time.Unix(1760000000, 0)
	s.Sample(0, t0) // 0 速起步：只建立基线
	if got := s.BPS(); got != 0 {
		t.Errorf("仅有基线点时 BPS = %v, 期望 0（尚无 Δdone/Δt）", got)
	}
}

// 首个读数直接落位（§3.12）：0 速起步后转入 100 BPS，第一个有效读数
// 就应钉在 100，而不是 α·instant≈12.5 那样从 0 爬坡。
func TestSpeedometerFirstReadingLands(t *testing.T) {
	s := NewSpeedometer()
	t0 := time.Unix(1760000000, 0)
	s.Sample(0, t0)                      // 基线
	feedConst(s, t0, 0, spdStep, 100, 0) // 一个采样点：done=20, Δt=200ms
	if got, wantLo, wantHi := s.BPS(), 99.0, 101.0; got < wantLo || got > wantHi {
		t.Errorf("首读应直接落位到 100 BPS（区间 [%.0f, %.0f]）, got %.2f；从 0 爬坡约为 12.5", wantLo, wantHi, got)
	}
}

// 恒速稳态：EWMA 落位后应钉在恒速值附近，不随采样点数漂移。
func TestSpeedometerSteadyStateHolds(t *testing.T) {
	s := NewSpeedometer()
	t0 := time.Unix(1760000000, 0)
	s.Sample(0, t0)
	feedConst(s, t0, 0, 4*time.Second, 100, 0) // 20 个采样点
	if got, wantLo, wantHi := s.BPS(), 99.0, 101.0; got < wantLo || got > wantHi {
		t.Errorf("恒速 100 BPS 跑 4s 后应稳定在 [%.0f, %.0f], got %.2f", wantLo, wantHi, got)
	}
}

// τ 的物理含义（§3.12）：100→400 BPS 阶跃后，约 τ（1.5s）追上 63%、
// 约 3τ 追上 95%。离散采样下取 8×200ms=1.6s≈τ、23×200ms=4.6s≈3τ。
func TestSpeedometerStepResponse(t *testing.T) {
	const v1, v2 = 100.0, 400.0 // 阶跃前/后恒速（BPS）
	s := NewSpeedometer()
	t0 := time.Unix(1760000000, 0)
	s.Sample(0, t0)
	done := feedConst(s, t0, 0, 2*time.Second, v1, 0) // 起步落位并稳定在 v1
	if got, wantLo, wantHi := s.BPS(), v1-1, v1+1; got < wantLo || got > wantHi {
		t.Fatalf("阶跃前应稳定在 %.0f BPS（区间 [%.0f, %.0f]）, got %.2f", v1, wantLo, wantHi, got)
	}

	for n := 1; n <= 23; n++ {
		done += int64(v2 * spdStep.Seconds()) // 每步 +80 字节
		s.Sample(done, t0.Add(2*time.Second+time.Duration(n)*spdStep))
		switch n {
		case 8: // 1.6s ≈ τ：期望 ≈ v1 + 0.632·Δv ≈ 290，给 ±带宽
			lo, hi := v1+0.58*(v2-v1), v1+0.70*(v2-v1)
			if got := s.BPS(); got < lo || got > hi {
				t.Errorf("阶跃后 ≈τ（1.6s）应追上 63%%（区间 [%.0f, %.0f] BPS）, got %.2f", lo, hi, got)
			}
		case 23: // 4.6s ≈ 3τ：应追上 ≥95%
			want := v1 + 0.95*(v2-v1)
			if got := s.BPS(); got < want {
				t.Errorf("阶跃后 ≈3τ（4.6s）应追上 ≥95%%（≥ %.0f BPS）, got %.2f", want, got)
			}
		}
	}
}

// Δt ≤ 0（同刻重复采样 / 时钟回拨）的采样点应被跳过：不 panic、
// 不产生 NaN/Inf，速度保持原值。
func TestSpeedometerSkipsNonPositiveDt(t *testing.T) {
	s := NewSpeedometer()
	t0 := time.Unix(1760000000, 0)
	s.Sample(0, t0)
	s.Sample(20, t0.Add(spdStep))               // 首读落位：100 BPS
	s.Sample(20, t0.Add(spdStep))               // 同刻重复：跳过
	s.Sample(999, t0.Add(100*time.Millisecond)) // 时钟回拨（早于 t0+200ms 基线点）：跳过
	if got := s.BPS(); math.IsNaN(got) || math.IsInf(got, 0) {
		t.Fatalf("无效 Δt 采样导致 BPS 非法: %v", got)
	}
	if got, want := s.BPS(), 100.0; got != want {
		t.Errorf("无效 Δt 的采样应被跳过，BPS 应保持 %.0f, got %v", want, got)
	}
}

// α 由 Δt 计算、与 200ms 节拍解耦：以 500ms 间隔采样恒速，同样收敛到真值。
func TestSpeedometerAdaptsToInterval(t *testing.T) {
	s := NewSpeedometer()
	t0 := time.Unix(1760000000, 0)
	s.Sample(0, t0)
	done := int64(0)
	for i := 1; i <= 12; i++ { // 500ms × 12 = 6s，恒速 100 BPS
		done += 50
		s.Sample(done, t0.Add(time.Duration(i)*500*time.Millisecond))
	}
	if got, wantLo, wantHi := s.BPS(), 99.0, 101.0; got < wantLo || got > wantHi {
		t.Errorf("500ms 间隔恒速 100 BPS 应收敛到 [%.0f, %.0f], got %.2f", wantLo, wantHi, got)
	}
}
