//go:build !windows

package watcher

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// fdLimit 返回本进程真正能打开的描述符数（RLIMIT_NOFILE 软限制）。
// 这与 sysctl 的 kern.maxfilesperproc / kern.maxfiles 不是一回事：后者是
// 系统给进程的天花板，前者才是本进程的额度。Go 运行时启动时会把软限制
// 抬到硬限制，所以这里读到的通常已是该进程可达的上限
func fdLimit() (int, bool) {
	var r unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &r); err != nil {
		return 0, false
	}
	// 某些系统上 RLIM_INFINITY 是个极大值，夹一下避免后续运算溢出
	if r.Cur > 1<<30 {
		return 1 << 30, true
	}
	return int(r.Cur), true
}

// openFDCount 返回本进程当前打开的描述符数。
// darwin 读 /dev/fd、linux 读 /proc/self/fd——两者都是「每个打开的描述符
// 一个条目」的目录。读取本身也要占一个描述符，结果因此偏大 1，对水位
// 判断无影响
func openFDCount() (int, bool) {
	dir := "/proc/self/fd"
	if runtime.GOOS == "darwin" {
		dir = "/dev/fd"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
	}
	return len(entries), true
}
