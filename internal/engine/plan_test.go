package engine

import (
	"testing"
	"time"
)

// 期望值全部按规格 §3.3 公式独立推演（另用脚本交叉核对），
// 而非简报 Step 1 注释中的手算值——后者与公式矛盾（见任务报告）。

const (
	tKB = int64(1 << 10)
	tMB = int64(1 << 20)
	tGB = int64(1 << 30)
)

func TestPlan(t *testing.T) {
	tests := []struct {
		name      string
		total     int64
		threads   int
		wantN     int   // 期望片数
		firstEnd  int64 // 第 0 片 End（Start 恒为 0）
		lastStart int64 // 最后一片 Start
		lastEnd   int64 // 最后一片 End（恒为 total-1）
	}{
		// 规格书 §3.3 原例：bySize=64, eff=512, want=min(512×32,16384)=16384,
		// seg=⌈9.5GB/16384⌉=622592, n=16384, finalSeg=622592（整除）
		{"规格例 9.5GB/512", int64(9.5 * float64(tGB)), 512, 16384, 622592 - 1, 16383 * 622592, 10200547328 - 1},

		// want=16384 但 ⌈4MB/16384⌉=256 被 MIN_SEG 抬到 512KB → n=⌈4MB/512KB⌉=8
		{"4MB/512 MIN_SEG 抬升", 4 * tMB, 512, 8, 512*tKB - 1, 7 * 512 * tKB, 4*tMB - 1},

		// want=max(16,64×32)=2048, ⌈100MB/2048⌉=51200 被夹到 512KB → n=200
		{"100MB/64", 100 * tMB, 64, 200, 512*tKB - 1, 199 * 512 * tKB, 100*tMB - 1},

		// want=max(32,32×32)=1024, seg=⌈1.5GB/1024⌉=1572864 在 [512KB,8MB] 内 → n=1024
		{"1.5GB/32", int64(1.5 * float64(tGB)), 32, 1024, 1572864 - 1, 1023 * 1572864, 1610612736 - 1},

		// 文件小于单片下限 → 1 片整文件
		{"300KB/1 单片", 300 * tKB, 1, 1, 300*tKB - 1, 0, 300*tKB - 1},

		// want=max(16,1×32)=32, ⌈100MB/32⌉=3276800 超 maxSeg=2MB 被压回 → seg=2MB, n=50
		{"100MB/1 maxSeg 压回", 100 * tMB, 1, 50, 2*tMB - 1, 49 * 2 * tMB, 100*tMB - 1},

		// 非整除：want=512 → seg 被 MIN_SEG 钉在 512KB → n=2, finalSeg=⌈1000003/2⌉=500002 重对齐
		{"1000003/16 非整除重对齐", 1000003, 16, 2, 500002 - 1, 500002, 1000003 - 1},

		// eff=min(4096,512)=512 → 结果与 512 档一致
		{"100MB/4096 eff 夹 512", 100 * tMB, 4096, 200, 512*tKB - 1, 199 * 512 * tKB, 100*tMB - 1},

		// 32MB 整（不属 <32MB 档）：bySize=16, maxSeg=2MB, want=32, seg=1MB → n=32
		{"32MB/1 档位边界", 32 * tMB, 1, 32, 1*tMB - 1, 31 * tMB, 32*tMB - 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			segs := Plan(tt.total, tt.threads)

			if len(segs) != tt.wantN {
				t.Fatalf("片数 = %d, want %d", len(segs), tt.wantN)
			}
			if segs[0].Start != 0 || segs[0].End != tt.firstEnd {
				t.Errorf("首片 = [%d,%d], want [0,%d]", segs[0].Start, segs[0].End, tt.firstEnd)
			}
			last := segs[len(segs)-1]
			if last.Start != tt.lastStart || last.End != tt.lastEnd {
				t.Errorf("末片 = [%d,%d], want [%d,%d]", last.Start, last.End, tt.lastStart, tt.lastEnd)
			}

			// 不变量：Index 连续、片间无缝闭区间、覆盖且仅覆盖 total 字节
			var sum int64
			for i, s := range segs {
				if s.Index != i {
					t.Fatalf("segs[%d].Index = %d", i, s.Index)
				}
				if s.Start > s.End || s.End >= tt.total {
					t.Fatalf("segs[%d] = [%d,%d] 非法（要求 Start≤End<total=%d）", i, s.Start, s.End, tt.total)
				}
				if i > 0 && segs[i-1].End+1 != s.Start {
					t.Fatalf("片间有缝/重叠: segs[%d].End+1=%d != segs[%d].Start=%d", i-1, segs[i-1].End+1, i, s.Start)
				}
				sum += s.End - s.Start + 1
			}
			if sum != tt.total {
				t.Errorf("Σ(End-Start+1) = %d, want %d", sum, tt.total)
			}
		})
	}
}

func TestPlanInvalidTotal(t *testing.T) {
	for _, total := range []int64{0, -1, -100 * tMB} {
		if segs := Plan(total, 8); len(segs) != 0 {
			t.Errorf("Plan(%d) 返回 %d 片, want 空", total, len(segs))
		}
	}
}

func TestBufSize(t *testing.T) {
	tests := []struct {
		n    int
		want int // max(64KB, min(256KB, 64MB/n))
	}{
		{1, 256 * 1024},   // 64MB/1=64MB 夹到 256KB
		{256, 256 * 1024}, // 64MB/256=256KB
		{300, 223696},     // 64MB/300=223696（区间内）
		{512, 128 * 1024}, // 64MB/512=128KB
		{1024, 64 * 1024}, // 64MB/1024=64KB
		{2048, 64 * 1024}, // 64MB/2048=32KB 被下限 64KB 兜住
	}
	for _, tt := range tests {
		if got := BufSize(tt.n); got != tt.want {
			t.Errorf("BufSize(%d) = %d, want %d", tt.n, got, tt.want)
		}
	}
}

func TestRamp(t *testing.T) {
	tests := []struct {
		n    int
		want time.Duration // n>256 ? 6s : 3s
	}{
		{1, 3 * time.Second},
		{256, 3 * time.Second},
		{257, 6 * time.Second},
		{16384, 6 * time.Second},
	}
	for _, tt := range tests {
		if got := Ramp(tt.n); got != tt.want {
			t.Errorf("Ramp(%d) = %v, want %v", tt.n, got, tt.want)
		}
	}
}
