//go:build windows

package engine

import (
	"syscall"
	"unsafe"
)

// diskFreeSpace 返回 dir 所在卷的可用空间（字节）。模式 B 磁盘预检用
// （§参数表：启动时校验剩余空间 ≥ 待下载量）。GetDiskFreeSpaceExW 的
// lpFreeBytesAvailable = 调用用户可用量（配额感知）。纯标准库：
// syscall.NewLazyDLL 动态绑定，零新增依赖。
var diskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

func diskFreeSpace(dir string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	r1, _, e := diskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&free)),
	)
	if r1 == 0 {
		return 0, e
	}
	return avail, nil
}
