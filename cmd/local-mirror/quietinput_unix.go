//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// quietInput 在实时刷新期间把 stdin 切成「不回显」：清掉 ECHO/ECHONL，
// 于是键入的字符、方向键与滚轮产生的转义序列都不再被终端回显到备用屏上。
// 刻意**只动回显位**：保留 ICANON（不改行编辑语义）、ISIG（Ctrl-C 仍生成
// SIGINT，交给上层信号退出）与 OPOST（保留 \n→\r\n 输出处理，否则整屏阶梯
// 错位）。返回还原函数；非 TTY 或 ioctl 失败时返回空操作，绝不影响主流程。
func quietInput() func() {
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return func() {}
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO | unix.ECHONL
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &quiet); err != nil {
		return func() {}
	}
	return func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) }
}
