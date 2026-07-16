//go:build !windows

package utils

import "golang.org/x/sys/unix"

// DiskFree 返回 path 所在文件系统上非特权用户可用的字节数（Bavail，
// 不含 root 保留块——普通用户真正写得进去的空间）
func DiskFree(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
