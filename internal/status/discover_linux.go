//go:build linux

package status

import (
	"os"
	"strconv"
	"strings"
)

// discoverProcRoots 遍历 /proc 找出 local-mirror 常驻进程，同步根从
// argv 的 -p（相对则挂 /proc/<pid>/cwd）或直接 cwd 推断。linux 上精确
func discoverProcRoots() []procRoot {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var out []procRoot
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		args := splitNUL(data)
		if !looksLikeDaemon(args) {
			continue
		}
		cwd, _ := os.Readlink("/proc/" + e.Name() + "/cwd")
		out = append(out, procRoot{PID: pid, Root: resolveRoot(args, cwd)})
	}
	return out
}

// splitNUL 拆 /proc/<pid>/cmdline 的 NUL 分隔 argv，去掉末尾空串
func splitNUL(b []byte) []string {
	parts := strings.Split(string(b), "\x00")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
