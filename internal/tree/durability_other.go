//go:build !linux && !windows

package tree

import (
	"os"
	"syscall"
)

// SyncFileData 用普通 fsync 把数据交给磁盘。os.File.Sync 在 macOS 上是 F_FULLFSYNC，
// 每次都清空磁盘缓存，逐文件调用代价约 9 ms；本批提交时数据库的 F_FULLFSYNC 会一并
// 清空磁盘缓存，此前 fsync 过的文件数据随之持久
func SyncFileData(f *os.File) error { return syscall.Fsync(int(f.Fd())) }

func syncQueuedData() error { return nil }
