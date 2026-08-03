//go:build !windows

package main

import (
	"io/fs"
	"syscall"
)

// fileOwnerUID 取文件属主 uid。syscall.Stat_t 是 Unix 专有的，
// 放进带 build tag 的文件里，否则 Windows 交叉编译直接失败
func fileOwnerUID(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
