//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import "golang.org/x/sys/unix"

// termios 读写 ioctl 请求码：macOS 与 BSD 用 TIOCGETA/TIOCSETA。
const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)
