//go:build !windows

package main

import (
	"syscall"
	"testing"
)

// TestRaiseFDLimit 裁决 3：soft 被抬高，或已达标（≥hard / ≥平台上限）；
// 且重复调用幂等、绝不降低。
func TestRaiseFDLimit(t *testing.T) {
	before, after, err := raiseFDLimit()
	if err != nil {
		t.Fatalf("raiseFDLimit() 报错: %v", err)
	}
	if after < before {
		t.Fatalf("soft 被降低: before=%d after=%d", before, after)
	}
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}
	if after == before {
		// no-op 分支：必须已达有效目标（soft ≥ hard，或 soft ≥ 平台上限）。
		ceiling := fdCeiling()
		if before < lim.Max && (ceiling == 0 || before < ceiling) {
			t.Fatalf("soft 既未抬高也未达标: before=%d after=%d max=%d ceiling=%d",
				before, after, lim.Max, ceiling)
		}
	}

	_, again, err := raiseFDLimit()
	if err != nil {
		t.Fatalf("raiseFDLimit() 二次调用报错: %v", err)
	}
	if again != after {
		t.Fatalf("raiseFDLimit 非幂等: 第一次 after=%d, 第二次=%d", after, again)
	}
}

// TestFDRaiseTarget 目标计算：min(hard, 平台上限)；已达标返回 0。
func TestFDRaiseTarget(t *testing.T) {
	cases := []struct {
		cur, max, want uint64
	}{
		{cur: 100, max: 200, want: 200}, // 常规：提到 hard
		{cur: 200, max: 200, want: 0},   // 已等于 hard
		{cur: 300, max: 200, want: 0},   // 超过 hard（异常防御，no-op）
		{cur: 0, max: 0, want: 0},       // 全零防御
	}
	for _, c := range cases {
		if got := fdRaiseTarget(c.cur, c.max); got != c.want {
			t.Fatalf("fdRaiseTarget(%d, %d) = %d, 期望 %d", c.cur, c.max, got, c.want)
		}
	}
	if ceiling := fdCeiling(); ceiling > 0 {
		// darwin：hard=unlimited 时必须封顶到平台上限而非直接提 hard。
		huge := uint64(1<<63 - 1)
		if got := fdRaiseTarget(100, huge); got != ceiling {
			t.Fatalf("fdRaiseTarget(100, unlimited) = %d, 期望封顶到 %d", got, ceiling)
		}
		// 但 hard 低于平台上限时仍以 hard 为准。
		if lim := ceiling - 1; lim > 100 {
			if got := fdRaiseTarget(100, lim); got != lim {
				t.Fatalf("fdRaiseTarget(100, %d) = %d, 期望 %d", lim, got, lim)
			}
		}
	}
}
