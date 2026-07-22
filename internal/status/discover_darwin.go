//go:build darwin

package status

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// discoverProcRoots 用 ps 列出进程并筛出 local-mirror 常驻进程。mac 无 /proc，
// 拿不到进程 cwd（不引入 lsof 那种重家伙），故只认**显式带 -p** 的实例——
// 服务化部署通常都显式指定同步根，够用；纯靠 cwd 默认根的临时进程发现不到
func discoverProcRoots() []procRoot {
	data, err := exec.Command("ps", "-axww", "-o", "pid=,args=").Output()
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var out []procRoot
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == self {
			continue
		}
		args := fields[1:]
		if !looksLikeDaemon(args) {
			continue
		}
		// 无 cwd：resolveRoot 只有拿到绝对 -p 才能给出可用根
		root := resolveRoot(args, "")
		if root == "" || !filepath.IsAbs(root) {
			continue
		}
		out = append(out, procRoot{PID: pid, Root: root})
	}
	return out
}
