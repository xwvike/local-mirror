package status

import (
	"path/filepath"
	"strings"
)

// Instance 本机上一个正在运行的 local-mirror 常驻实例
type Instance struct {
	PID  int
	Root string
	Snap *Snapshot
}

// procRoot 进程表里一个疑似 local-mirror 常驻进程及其推断出的同步根
type procRoot struct {
	PID  int
	Root string
}

// DiscoverInstances 扫描本机进程表，找出所有正在运行的 local-mirror 常驻实例。
// 不建任何注册表——「进程表 + 各同步根下的 status.json」本身就是事实来源：
// 进程给出候选根（-p 或 cwd），根下的 status.json 给出运行时状态，两者的 pid
// 必须吻合才算数（排除同目录里旧实例残留的快照）。纯只读，不影响任何进程。
// 平台实现见 discover_{linux,darwin,other}.go；无法枚举进程的平台返回空
func DiscoverInstances() []Instance {
	var out []Instance
	seen := make(map[string]bool)
	for _, pr := range discoverProcRoots() {
		if pr.Root == "" || seen[pr.Root] {
			continue
		}
		snap, err := Load(pr.Root)
		if err != nil || snap == nil {
			continue
		}
		// 交叉校验：快照里的 pid 必须与进程表里的 pid 一致，
		// 否则是同目录下前一个已死实例留下的旧文件
		if snap.PID != pr.PID {
			continue
		}
		seen[pr.Root] = true
		out = append(out, Instance{PID: pr.PID, Root: pr.Root, Snap: snap})
	}
	return out
}

// looksLikeDaemon 判断一条 argv 是否是 local-mirror 的常驻同步进程：
// 二进制名须为 local-mirror（发布名恒定），且不带任何"读完即退"的旗子
// （--status/--gen-key/--show-key/--version/--help），那些不是常驻进程
func looksLikeDaemon(args []string) bool {
	if len(args) == 0 || filepath.Base(args[0]) != "local-mirror" {
		return false
	}
	for _, a := range args {
		switch a {
		case "--status", "--gen-key", "--show-key", "--version", "-v", "--help", "-h":
			return false
		}
	}
	return true
}

// resolveRoot 从进程 argv 与 cwd 推断同步根：-p/--path 优先（相对路径挂到
// cwd 上），未指定则同步根就是 cwd（与 resolveSyncRoot 的默认一致）
func resolveRoot(args []string, cwd string) string {
	p := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-p" || a == "--path":
			if i+1 < len(args) {
				p = args[i+1]
			}
		case strings.HasPrefix(a, "-p="):
			p = a[len("-p="):]
		case strings.HasPrefix(a, "--path="):
			p = a[len("--path="):]
		}
	}
	if p == "" {
		return cwd
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if cwd == "" {
		return "" // 相对 -p 但拿不到 cwd（如 mac 上无 /proc）：无法解析
	}
	return filepath.Join(cwd, p)
}
