//go:build !windows

package main

import "syscall"

// fdRaiseTarget 计算本次应提的 soft 目标：min(hard, 平台上限)；返回 0
// 表示 cur 已达标（无需提升）。纯函数便于测试。
func fdRaiseTarget(cur, max uint64) uint64 {
	target := max
	if c := fdCeiling(); c > 0 && target > c {
		target = c
	}
	if cur >= target {
		return 0
	}
	return target
}

// raiseFDLimit 自提 RLIMIT_NOFILE 的 soft → hard（选型报告 §5.2：512 连
// 发 + 每片文件句柄，macOS 默认 soft 256 不够；跨平台同一处调用，Windows
// 由 rlimit_windows.go 编译期跳过）。
//
// 目标取 min(hard, 平台上限)：darwin 的 hard 常为 unlimited（0x7FFF...），
// 直接提 hard 会超 kern.maxfilesperproc 而 EINVAL，故以 sysctl 封顶；
// 封顶仍失败时回退保守值 10240（仍远高于默认 256）。任何失败都不改当前
// 值，交调用方记日志继续。
func raiseFDLimit() (before, after uint64, err error) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, 0, err
	}
	before = lim.Cur

	target := fdRaiseTarget(lim.Cur, lim.Max)
	if target == 0 {
		return before, before, nil // 已达标（≥hard 或 ≥平台上限）
	}
	if err := setNoFile(target); err == nil {
		return before, target, nil
	}

	const fallback uint64 = 10240
	if lim.Cur >= fallback {
		return before, lim.Cur, nil
	}
	if err := setNoFile(fallback); err != nil {
		return before, lim.Cur, err // 保持原值
	}
	return before, fallback, nil
}

// setNoFile 只提 soft 到 cur（hard 原样透传）。
func setNoFile(cur uint64) error {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return err
	}
	lim.Cur = cur
	return syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim)
}
