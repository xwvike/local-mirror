package tree

import (
	"local-mirror/config"
	"os"

	"golang.org/x/sys/unix"
)

// SyncFileData Linux 上不逐文件 fsync：提交前对整个文件系统做一次 syncfs
func SyncFileData(*os.File) error { return nil }

func syncQueuedData() error {
	f, err := os.Open(config.StartPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Syncfs(int(f.Fd()))
}
