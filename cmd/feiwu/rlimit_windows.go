//go:build windows

package main

// raiseFDLimit Windows 编译跳过（选型报告 §5.2：Win32 句柄不占 POSIX fd
// 配额）：no-op，返回零值。
func raiseFDLimit() (before, after uint64, err error) { return 0, 0, nil }
