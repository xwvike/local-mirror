//go:build linux

package status

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// sampleProc 采集本进程资源：CPU 时间走 getrusage，当前 RSS 与 fd 数走
// /proc/self（linux 上都是精确当前值，也是生产端所在平台）
func sampleProc() procStats {
	ps := procStats{FDs: -1}

	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err == nil {
		ps.CPUSeconds = float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 +
			float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
	}

	// /proc/self/statm 第二字段 = 常驻页数
	if data, err := os.ReadFile("/proc/self/statm"); err == nil {
		if f := strings.Fields(string(data)); len(f) >= 2 {
			if pages, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				ps.RSS = pages * uint64(os.Getpagesize())
				ps.HasRSS = true
			}
		}
	}

	// /proc/self/fd 的条目数 = 打开的文件描述符数
	if entries, err := os.ReadDir("/proc/self/fd"); err == nil {
		ps.FDs = len(entries)
		ps.HasFDs = true
	}

	return ps
}
