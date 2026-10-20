package utils

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic 以 perm 权限原子地写入 path：先写同目录下的临时文件并 fsync，
// 再 rename 覆盖。中途失败（最常见是磁盘满）时原文件原封不动。
// os.WriteFile 先截断再写，失败就把原内容一起毁掉——对服务配置（含各任务口令）、
// 密钥文件这类唯一副本是不可接受的。结果文件的权限恒为 perm，不沿用旧文件的权限
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
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
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	ok = true
	return nil
}
