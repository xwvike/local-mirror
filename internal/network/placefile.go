package network

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// renameFile 测试可替换，用来模拟跨文件系统的 rename 失败
var renameFile = os.Rename

// placeFile 把校验过的分片换到目标位置。分片放在同步根的 .local-mirror/partial 下，
// 目标所在子目录若是另一个挂载点，rename 跨不过文件系统，这个文件就永远落不了盘。
// 跨设备时改为先复制到目标目录里的临时文件并落盘，再在目标文件系统内 rename：
// 替换依旧原子，读者看不到写了一半的文件
func placeFile(partialPath, fullPath string) error {
	err := renameFile(partialPath, fullPath)
	if err == nil || !crossDevice(err) {
		return err
	}
	if err := copyIntoPlace(partialPath, fullPath); err != nil {
		return err
	}
	return os.Remove(partialPath)
}

// crossDevice rename 是否因跨文件系统失败（Windows 跨卷是 ERROR_NOT_SAME_DEVICE）
func crossDevice(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	if runtime.GOOS == "windows" {
		return errno == 17 // ERROR_NOT_SAME_DEVICE
	}
	return errno == syscall.EXDEV
}

func copyIntoPlace(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".lm-tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	ok = true
	return nil
}
