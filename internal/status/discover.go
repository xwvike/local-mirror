package status

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
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

// valueFlags 带值的旗子：其后的 argv 是值，不是位置参数
var valueFlags = map[string]bool{
	"p": true, "path": true, "m": true, "mode": true, "r": true, "realityip": true,
	"k": true, "secret": true, "l": true, "loglevel": true, "c": true, "cooldown": true,
	"f": true, "filebuffersize": true, "a": true, "alias": true, "i": true, "ignore": true,
	"config": true, "connect": true,
}

// resolveRoot 从进程 argv 与 cwd 推断同步根，与 main 的解析口径一致：
// -p/--path 优先；--config 只有单任务（进程内直跑）时取该任务的 path，多任务的
// 监督进程本身不是实例（子进程带 -p，各自被发现）；位置形态取不带 @ 的那一侧；
// 都没有则同步根就是 cwd。相对路径挂到 cwd 上，拿不到 cwd 时无解
func resolveRoot(args []string, cwd string) string {
	var p, cfg string
	var positional []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = args[i+1:]
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = args[i:]
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !hasVal && valueFlags[name] && i+1 < len(args) {
			i++
			val = args[i]
		}
		switch name {
		case "p", "path":
			p = val
		case "config":
			cfg = val
		}
	}
	switch {
	case p != "":
	case cfg != "":
		if p = singleTaskPath(absUnder(cwd, cfg)); p == "" {
			return ""
		}
	case len(positional) == 2:
		p = positional[0]
		if strings.HasPrefix(p, "@") {
			p = positional[1]
		}
	default:
		return cwd
	}
	return absUnder(cwd, p)
}

// absUnder 把 p 解析为绝对路径（相对则挂到 cwd）；相对且无 cwd 时返回空
func absUnder(cwd, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if cwd == "" {
		return ""
	}
	return filepath.Join(cwd, p)
}

// singleTaskPath 读 YAML 配置，恰好一个任务时返回其 path（原样，相对路径由调用方
// 按进程 cwd 解析，与守护进程 LoadMultiConfig 的 filepath.Abs 同口径）。
// 读不到、多任务或解析失败都返回空
func singleTaskPath(cfgPath string) string {
	if cfgPath == "" {
		return ""
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return ""
	}
	var cfg struct {
		Tasks []struct {
			Path string `yaml:"path"`
		} `yaml:"tasks"`
	}
	if yaml.Unmarshal(data, &cfg) != nil || len(cfg.Tasks) != 1 {
		return ""
	}
	return cfg.Tasks[0].Path
}
