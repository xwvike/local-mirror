//go:build windows

package appError

import (
	"errors"
	"syscall"
)

// Windows 上写满磁盘返回的两个系统错误码
const (
	errorDiskFull       = syscall.Errno(112) // ERROR_DISK_FULL
	errorHandleDiskFull = syscall.Errno(39)  // ERROR_HANDLE_DISK_FULL
)

// IsDiskFull 判断错误链中是否为"磁盘空间不足"类系统错误
func IsDiskFull(err error) bool {
	return errors.Is(err, errorDiskFull) || errors.Is(err, errorHandleDiskFull)
}
