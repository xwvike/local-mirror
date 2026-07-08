package main

import (
	"flag"
	"fmt"
	"local-mirror/config"
	app "local-mirror/internal"
	"local-mirror/internal/logger"
	"local-mirror/internal/network"
	"local-mirror/internal/safety"
	"local-mirror/internal/tree"
	"local-mirror/pkg/utils"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// version 可在构建时注入: go build -ldflags "-X main.version=v1.2.3"
var version = "dev"

func init() {
	config.InstanceID = utils.GenerateRandomNum()
	config.StartTime = time.Now().Unix()
}

// resolveSyncRoot 确定同步根目录：-p 优先，否则当前工作目录；
// 必须是已存在的目录，返回绝对路径
func resolveSyncRoot() (string, error) {
	root := *config.Path
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("获取当前工作目录失败: %v", err)
		}
		root = wd
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("无法解析路径 %q: %v", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("同步目录不存在: %s", abs)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("同步路径不是目录: %s", abs)
	}
	return abs, nil
}

func main() {
	flag.Parse()

	// 用户主动请求帮助：输出到 stdout，退出码 0
	if *config.Help {
		config.PrintUsage(os.Stdout)
		os.Exit(0)
	}

	if *config.Version {
		fmt.Printf("local-mirror %s\n", version)
		fmt.Printf("protocol: %d\n", config.ProtocolVersion)
		fmt.Printf("go: %s (%s/%s)\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	// 用法错误：信息到 stderr，退出码 2（与 flag 包解析失败时的约定一致）
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "local-mirror: 未知参数: %v\n请使用 --help 查看用法\n", flag.Args())
		os.Exit(2)
	}
	if _, ok := config.ModeMap[*config.Mode]; !ok {
		fmt.Fprintf(os.Stderr, "local-mirror: 无效的运行模式 %q (可选: reality, mirror, relay)\n", *config.Mode)
		os.Exit(2)
	}
	switch *config.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		fmt.Fprintf(os.Stderr, "local-mirror: 无效的日志级别 %q (可选: debug, info, warn, error)\n", *config.LogLevel)
		os.Exit(2)
	}
	// 中继必须显式指定上游：留空回退 127.0.0.1 几乎必然是配置错误
	// （本机 127.0.0.1 上大概率只有它自己的服务端）
	if *config.Mode == "relay" && *config.RealityIP == "" {
		fmt.Fprintf(os.Stderr, "local-mirror: relay 模式必须用 -r 指定上游服务器地址\n")
		os.Exit(2)
	}

	root, err := resolveSyncRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
		os.Exit(2)
	}
	config.StartPath = root

	// 启用删除的同步方（mirror/relay）不得运行在关键路径上：
	// 即使用户主动加了 --allow-delete，也用真实路径（解引用后）拒绝在
	// ~、/、系统目录等位置删除，作为与用户意图无关的兜底防线
	if *config.AllowDelete && config.SyncsFromUpstream() {
		if err := safety.CheckDeletableRoot(root); err != nil {
			fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
			os.Exit(2)
		}
	}

	logger.InitLogger()

	// 先取目录锁（bbolt 文件锁，同目录单实例互斥），再绑定端口、打印横幅。
	// 顺序反了会出现"横幅宣布成功后才因锁退出"的误导，以及一个
	// accept 循环永远不会启动的幽灵端口
	tree.InitDB()
	defer func() {
		if tree.DB != nil {
			if err := tree.DB.Close(); err != nil {
				log.Errorf("关闭数据库时出错: %v", err)
			}
		}
	}()

	// 对下游提供服务的模式（reality/relay）在打印横幅前先绑定端口
	// （从 DefaultPort 起自动探测），横幅里展示的才是真实监听端口；
	// accept 循环稍后由 Reality 启动
	if config.ServesDownstream() {
		listener, port, err := network.ListenAvailable(config.DefaultPort, config.PortScanRange)
		if err != nil {
			fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
			os.Exit(1)
		}
		app.ServerListener = listener
		config.ActualPort = port
	}

	printBanner()
	log.Infof("启动: version=%s mode=%s instance=%08x root=%s", version, *config.Mode, config.InstanceID, config.StartPath)

	app.App()
}

// ANSI 颜色码，仅在输出到终端时启用
type palette struct {
	bold, dim, cyan, green, reset string
}

func newPalette() palette {
	// 遵守 NO_COLOR 约定 (https://no-color.org)；管道/重定向时输出纯文本
	fi, err := os.Stdout.Stat()
	isTTY := err == nil && fi.Mode()&os.ModeCharDevice != 0
	if !isTTY || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return palette{}
	}
	return palette{
		bold:  "\033[1m",
		dim:   "\033[2m",
		cyan:  "\033[36m",
		green: "\033[32m",
		reset: "\033[0m",
	}
}

// displayWidth 计算终端显示宽度：CJK 字符占两列，ASCII 占一列
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if r >= 0x2E80 {
			w += 2
		} else {
			w++
		}
	}
	return w
}

// printBanner 向 stdout 输出启动状态。
// 长驻进程默认日志级别下终端不应完全静默，用户需要知道进程在做什么
func printBanner() {
	p := newPalette()
	const width = 48
	const labelWidth = 10

	line := p.dim + strings.Repeat("─", width) + p.reset
	row := func(label, value string) {
		pad := strings.Repeat(" ", max(1, labelWidth-displayWidth(label)))
		fmt.Printf("  %s%s%s%s%s\n", p.dim, label, p.reset, pad, value)
	}

	modeDescMap := map[string]string{"reality": "服务器", "mirror": "客户端", "relay": "中继"}
	modeDesc := modeDescMap[*config.Mode]

	fmt.Println(line)
	fmt.Printf("  %s%sLocal Mirror%s %s  ·  %s%s%s (%s)\n",
		p.bold, p.cyan, p.reset, version, p.bold, *config.Mode, p.reset, modeDesc)
	fmt.Println(line)
	row("同步目录", config.StartPath)
	if config.SyncsFromUpstream() {
		ip := *config.RealityIP
		if ip == "" {
			ip = "127.0.0.1"
		}
		row("上游服务器", fmt.Sprintf("%s%s%s %s(端口探测 %d-%d)%s",
			p.green, ip, p.reset, p.dim, config.DefaultPort, config.DefaultPort+config.PortScanRange-1, p.reset))
	}
	if config.ServesDownstream() {
		row("监听地址", fmt.Sprintf("%s0.0.0.0:%d%s", p.green, config.ActualPort, p.reset))
	}
	if *config.Secret != "" {
		row("传输加密", fmt.Sprintf("%s开启%s (Noise NNpsk0)", p.green, p.reset))
	} else {
		row("传输加密", fmt.Sprintf("关闭 %s(明文传输，可用 -k 开启)%s", p.dim, p.reset))
	}
	// 仅同步方（mirror/relay）涉及删除，展示当前删除策略
	if config.SyncsFromUpstream() {
		if *config.AllowDelete {
			row("删除同步", fmt.Sprintf("%s开启%s %s(忠实镜像，会删除本地多余文件)%s", p.green, p.reset, p.dim, p.reset))
		} else {
			row("删除同步", fmt.Sprintf("关闭 %s(仅增量，本地多余文件保留)%s", p.dim, p.reset))
		}
	}
	row("实例 ID", fmt.Sprintf("%08x", config.InstanceID))
	row("进程 PID", fmt.Sprintf("%d", os.Getpid()))
	row("日志", fmt.Sprintf("%s %s(级别 %s)%s", logger.LogPath(), p.dim, *config.LogLevel, p.reset))
	fmt.Println(line)
}
