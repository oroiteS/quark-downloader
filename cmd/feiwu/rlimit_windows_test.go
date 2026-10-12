//go:build windows

package main

import "testing"

// TestRaiseFDLimitNoop Windows 编译跳过（裁决 3）：raiseFDLimit 为 no-op。
func TestRaiseFDLimitNoop(t *testing.T) {
	before, after, err := raiseFDLimit()
	if err != nil {
		t.Fatalf("raiseFDLimit() 报错: %v", err)
	}
	if before != 0 || after != 0 {
		t.Fatalf("Windows no-op 应返回零值: before=%d after=%d", before, after)
	}
}
