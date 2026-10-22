package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const clkTck = 100

var pageSize = int64(os.Getpagesize())

func snapshotProcs() (map[int]procInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	procs := map[int]procInfo{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// comm 可能含空格，字段从最后一个 ')' 之后开始数
		s := string(data)
		f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(f) < 22 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		utime, _ := strconv.ParseInt(f[11], 10, 64)
		stime, _ := strconv.ParseInt(f[12], 10, 64)
		rss, _ := strconv.ParseInt(f[21], 10, 64)
		procs[pid] = procInfo{
			PPID: ppid,
			RSS:  rss * pageSize,
			CPU:  time.Duration(utime+stime) * time.Second / clkTck,
		}
	}
	return procs, nil
}

// maxRSS linux 的 ru_maxrss 单位是 KiB
func maxRSS(ru *syscall.Rusage) int64 { return ru.Maxrss * 1024 }

// listening 本机是否有进程在 TCP port 上监听（/proc/net/tcp{,6} 里状态 0A）
func listening(port int) bool {
	want := fmt.Sprintf(":%04X", port)
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			fs := strings.Fields(line)
			if len(fs) > 3 && strings.HasSuffix(fs[1], want) && fs[3] == "0A" {
				return true
			}
		}
	}
	return false
}
