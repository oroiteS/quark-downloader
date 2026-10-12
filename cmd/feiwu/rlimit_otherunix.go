//go:build !windows && !darwin

package main

// fdCeiling 非 darwin unix 无进程级 sysctl 配额（如 Linux 由 hard limit
// 直接约束）：0 表示不封顶。
func fdCeiling() uint64 { return 0 }
