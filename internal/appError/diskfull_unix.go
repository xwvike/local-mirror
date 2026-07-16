//go:build !windows

package appError

import (
	"errors"
	"syscall"
)

// IsDiskFull 判断错误链中是否为"磁盘空间不足"类系统错误。
// EDQUOT（配额耗尽）对写入方的表现与 ENOSPC 一致，一并识别
func IsDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
