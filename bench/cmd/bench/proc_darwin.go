package main

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func snapshotProcs() (map[int]procInfo, error) {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,rss=,time=").Output()
	if err != nil {
		return nil, err
	}
	procs := map[int]procInfo{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 4 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		rss, e3 := strconv.ParseInt(f[2], 10, 64)
		cpu, e4 := parsePSTime(f[3])
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			continue
		}
		procs[pid] = procInfo{PPID: ppid, RSS: rss * 1024, CPU: cpu}
	}
	return procs, nil
}

// parsePSTime 解析 ps 的 time 列：[[hh:]mm:]ss.cc
func parsePSTime(s string) (time.Duration, error) {
	var total float64
	for _, part := range strings.Split(s, ":") {
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, err
		}
		total = total*60 + v
	}
	return time.Duration(total * float64(time.Second)), nil
}

// maxRSS darwin 的 ru_maxrss 单位是字节
func maxRSS(ru *syscall.Rusage) int64 { return ru.Maxrss }
