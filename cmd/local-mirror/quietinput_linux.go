//go:build linux

package main

import "golang.org/x/sys/unix"

// termios 读写 ioctl 请求码：Linux 用 TCGETS/TCSETS。
const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)
