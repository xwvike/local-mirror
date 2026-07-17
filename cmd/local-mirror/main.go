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
	"local-mirror/internal/tui"
	"local-mirror/pkg/termstyle"
	"local-mirror/pkg/utils"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/term"
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

// discoveryWindow 单轮 UDP 扫描的收集窗口
const discoveryWindow = 2 * time.Second

// runDiscovery 扫描局域网服务端并确定上游地址，写入
// config.DiscoveredAddr/DiscoveredAlias 后返回。
// 交互终端下始终展示列表让用户确认（哪怕只发现一台，避免连错）；
// 非终端（systemd/管道）下恰好一台才自动连接。零台 exit 1——上游可能
// 只是还没启动（开机顺序），属可重试的暂时状态，监督进程/systemd 会
// 退避重启再扫；多台 exit 2——配置歧义，重试无解，必须 -r 显式指定。
// 失败路径全部在本函数内 os.Exit
func runDiscovery() {
	isTTY := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	for {
		if isTTY {
			fmt.Printf("正在扫描局域网服务端（%s）…\n", discoveryWindow)
		}
		servers, err := network.DiscoverServers(discoveryWindow, *config.Secret, config.InstanceID)
		if isTTY {
			// 扫描结束后擦掉进度行，选择列表（或横幅）原地出现，不留残余
			fmt.Print("\x1b[1A\x1b[K")
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "local-mirror: 自动发现失败: %v\n请用 -r 指定上游服务器地址\n", err)
			os.Exit(2)
		}

		if !isTTY {
			switch len(servers) {
			case 1:
				config.DiscoveredAddr = servers[0].Addr()
				config.DiscoveredAlias = servers[0].Alias
				log.Infof("自动发现上游: %s (%s)", servers[0].Addr(), servers[0].Alias)
				return
			case 0:
				fmt.Fprintf(os.Stderr, "local-mirror: 未发现局域网服务端（上游未启动？稍后可重试），"+
					"或用 -r 显式指定\n（VPN、跨网段或防火墙环境不支持自动发现）\n")
				os.Exit(1)
			default:
				fmt.Fprintf(os.Stderr, "local-mirror: 发现 %d 个服务端，非交互环境无法自动选择，请用 -r 指定:\n", len(servers))
				for _, s := range servers {
					fmt.Fprintf(os.Stderr, "  %-20s %-21s %s\n", s.Alias, s.Addr(), s.SyncPath)
				}
				os.Exit(2)
			}
		}

		opts := make([]tui.Option, len(servers))
		for i, s := range servers {
			opts[i] = tui.Option{Alias: s.Alias, Addr: s.Addr(), Path: s.SyncPath}
		}
		idx, outcome, err := tui.Select(fmt.Sprintf("发现 %d 个 local-mirror 服务端:", len(servers)), opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
			os.Exit(1)
		}
		switch outcome {
		case tui.Rescan:
			continue
		case tui.Canceled:
			os.Exit(130) // 128+SIGINT，用户主动取消
		case tui.Selected:
			config.DiscoveredAddr = servers[idx].Addr()
			config.DiscoveredAlias = servers[idx].Alias
			return
		}
	}
}

func main() {
	// 尽早切换控制台代码页：--help/--version 与用法错误的输出同样是中文。
	// os.Exit 的快速退出路径不经 defer，代码页会留在 UTF-8——两害相权：
	// 留下 65001 只影响极老的 GBK 输出程序，而不切换则本程序全部输出乱码
	restoreConsole := enableConsoleUTF8()
	defer restoreConsole()

	// SIGUSR1 → 目录热度快照落盘（观察用，见 sigdump_unix.go）
	installHeatDumpSignal()

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

	// 多任务监督模式：--config 与单实例旗子互斥，避免"以为在配置任务
	// 实际全被忽略"的误会
	if *config.ConfigFile != "" {
		var extra []string
		flag.Visit(func(f *flag.Flag) {
			if f.Name == "config" {
				return
			}
			dash := "--"
			if len(f.Name) == 1 {
				dash = "-"
			}
			extra = append(extra, dash+f.Name)
		})
		if len(extra) > 0 {
			fmt.Fprintf(os.Stderr, "local-mirror: --config 模式下其余参数无效: %v\n", extra)
			os.Exit(2)
		}
		multiCfg, err := config.LoadMultiConfig(*config.ConfigFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
			os.Exit(2)
		}
		if n := countRealityTasks(multiCfg); n > config.PortScanRange {
			fmt.Fprintf(os.Stderr, "local-mirror: 警告: %d 个服务端任务超过端口探测范围（%d），超出部分将无法绑定端口\n",
				n, config.PortScanRange)
		}
		runSupervisor(multiCfg) // 不返回
		return
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
	root, err := resolveSyncRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
		os.Exit(2)
	}
	config.StartPath = root

	// 三级安全阶梯（对所有同步方生效，不再只在 --allow-delete 时检查）：
	// 关键路径（~、/、系统目录，真实路径解引用后判定）默认连"只同步"都拒绝
	// ——因为同步会覆盖已存在文件；须 --allow-critical 显式解锁，解锁后开启
	// 覆盖前快照备份。删除仍由 --allow-delete 单独控制
	if config.SyncsFromUpstream() {
		snapshot, err := safety.CheckSyncSafety(root, *config.AllowCritical)
		if err != nil {
			fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
			os.Exit(2)
		}
		config.SnapshotOverwrites = snapshot
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

	// 忽略列表：内置默认 + -i 旗子 + .local-mirror/ignore 文件合并。
	// 必须在 InitDB 之后（状态目录已建）、BuildFileTree/watcher 启动之前
	if err := config.LoadIgnoreList(config.StartPath); err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
		os.Exit(2)
	}

	// 实例别名（服务端在局域网发现中广播）：--alias → 主机名 → 兜底
	config.AliasName = *config.Alias
	if config.AliasName == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			config.AliasName = h
		} else {
			config.AliasName = "local-mirror"
		}
	}

	// -r 留空的同步方（mirror/relay）先自动发现上游再继续启动。
	// 必须在 InitDB（单实例锁）之后：否则用户选完服务器才因目录被占退出。
	// 中继此刻自己的发现应答器尚未启动，结构上不会扫到自己
	if config.SyncsFromUpstream() && *config.RealityIP == "" {
		runDiscovery()
	}

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

		// UDP 发现应答器：失败不致命（客户端仍可 -r 直连）
		if _, err := network.StartDiscoveryResponder(port, config.AliasName, config.StartPath, *config.Secret); err != nil {
			log.Warnf("UDP 服务发现应答器启动失败（客户端可用 -r 直连）: %v", err)
		}
	}

	printBanner()
	log.Infof("启动: version=%s mode=%s instance=%08x root=%s", version, *config.Mode, config.InstanceID, config.StartPath)

	app.App()
}

// printBanner 向 stdout 输出启动状态。
// 长驻进程默认日志级别下终端不应完全静默，用户需要知道进程在做什么
func printBanner() {
	p := termstyle.NewPalette(os.Stdout)
	const width = 48
	const labelWidth = 10

	line := p.Dim + strings.Repeat("─", width) + p.Reset
	row := func(label, value string) {
		pad := strings.Repeat(" ", max(1, labelWidth-termstyle.DisplayWidth(label)))
		fmt.Printf("  %s%s%s%s%s\n", p.Dim, label, p.Reset, pad, value)
	}

	modeDescMap := map[string]string{"reality": "服务器", "mirror": "客户端", "relay": "中继"}
	modeDesc := modeDescMap[*config.Mode]

	fmt.Println(line)
	fmt.Printf("  %s%sLocal Mirror%s %s  ·  %s%s%s (%s)\n",
		p.Bold, p.Cyan, p.Reset, version, p.Bold, *config.Mode, p.Reset, modeDesc)
	fmt.Println(line)
	row("同步目录", config.StartPath)
	// 忽略规则最多展示 4 条，其余折叠为计数（完整列表见 --help 与配置）
	ignoreShown := config.IgnoreFileList
	suffix := ""
	if len(ignoreShown) > 4 {
		suffix = fmt.Sprintf(" %s(+%d)%s", p.Dim, len(ignoreShown)-4, p.Reset)
		ignoreShown = ignoreShown[:4]
	}
	row("忽略规则", strings.Join(ignoreShown, ", ")+suffix)
	if config.SyncsFromUpstream() {
		switch {
		case config.DiscoveredAddr != "":
			row("上游服务器", fmt.Sprintf("%s%s%s %s(自动发现: %s)%s",
				p.Green, config.DiscoveredAddr, p.Reset, p.Dim, config.DiscoveredAlias, p.Reset))
		default:
			ip := *config.RealityIP
			if ip == "" {
				ip = "127.0.0.1"
			}
			row("上游服务器", fmt.Sprintf("%s%s%s %s(端口探测 %d-%d)%s",
				p.Green, ip, p.Reset, p.Dim, config.DefaultPort, config.DefaultPort+config.PortScanRange-1, p.Reset))
		}
	}
	if config.ServesDownstream() {
		row("监听地址", fmt.Sprintf("%s0.0.0.0:%d%s", p.Green, config.ActualPort, p.Reset))
	}
	if *config.Secret != "" {
		row("传输加密", fmt.Sprintf("%s开启%s (Noise NNpsk0)", p.Green, p.Reset))
	} else {
		row("传输加密", fmt.Sprintf("关闭 %s(明文传输，可用 -k 开启)%s", p.Dim, p.Reset))
	}
	// 仅同步方（mirror/relay）涉及删除，展示当前删除策略
	if config.SyncsFromUpstream() {
		if *config.AllowDelete {
			row("删除同步", fmt.Sprintf("%s开启%s %s(忠实镜像，会删除本地多余文件)%s", p.Green, p.Reset, p.Dim, p.Reset))
		} else {
			row("删除同步", fmt.Sprintf("关闭 %s(仅增量，本地多余文件保留)%s", p.Dim, p.Reset))
		}
		// 关键路径解锁档：提示覆盖前会快照备份
		if config.SnapshotOverwrites {
			row("关键路径", fmt.Sprintf("%s已解锁%s %s(--allow-critical，首次覆盖备份到 .local-mirror/backups)%s",
				p.Green, p.Reset, p.Dim, p.Reset))
		}
	}
	row("实例 ID", fmt.Sprintf("%08x", config.InstanceID))
	row("进程 PID", fmt.Sprintf("%d", os.Getpid()))
	row("日志", fmt.Sprintf("%s %s(级别 %s)%s", logger.LogPath(), p.Dim, *config.LogLevel, p.Reset))
	fmt.Println(line)
}
