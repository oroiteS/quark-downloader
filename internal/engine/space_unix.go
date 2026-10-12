//go:build !windows

package engine

import "syscall"

// diskFreeSpace 返回 dir 所在文件系统的可用空间（字节）。模式 B 磁盘
// 预检用（§参数表：启动时校验剩余空间 ≥ 待下载量）。Bavail = 普通用户
// 可用量（预留块不算），Bsize = 块大小。
func diskFreeSpace(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
