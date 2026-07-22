//go:build darwin

package status

import "syscall"

// sampleProc 采集本进程资源。CPU 时间走 getrusage；darwin 无 cgo 时拿不到
// 当前 RSS 与 fd 数的轻量接口，故 RSS 用 getrusage 的峰值近似（darwin 上
// ru_maxrss 单位是字节），fd 数标未知（-1）。生产端是 linux，那里两者精确
func sampleProc() procStats {
	ps := procStats{FDs: -1}

	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err == nil {
		ps.CPUSeconds = float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 +
			float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
		// darwin: ru_maxrss 以字节计，为进程生命周期内的峰值常驻集
		ps.RSS = uint64(ru.Maxrss)
		ps.HasRSS = true
	}

	return ps
}
