//go:build unix

package tree

import "golang.org/x/sys/unix"

// mkfifo 创建具名管道，仅供测试构造非普通文件
func mkfifo(path string) error { return unix.Mkfifo(path, 0o600) }
