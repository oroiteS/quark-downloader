//go:build darwin

package main

import "syscall"

// fdCeiling darwin 的进程级 fd 配额（kern.maxfilesperproc）：hard 常为
// unlimited（0x7FFF...），Setrlimit 直接提到 hard 会超配额 EINVAL，故
// 以 sysctl 封顶。读不到时返回 0（raiseFDLimit 以 10240 兜底）。
func fdCeiling() uint64 {
	m, err := syscall.SysctlUint32("kern.maxfilesperproc")
	if err != nil || m == 0 {
		return 0
	}
	return uint64(m)
}
