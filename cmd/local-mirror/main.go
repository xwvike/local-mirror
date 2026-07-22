package main

import (
	"flag"
	"fmt"
	"local-mirror/config"
	app "local-mirror/internal"
	"local-mirror/internal/keyfile"
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
			return "", fmt.Errorf("failed to get working directory: %v", err)
		}
		root = wd
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("cannot resolve path %q: %v", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("sync directory does not exist: %s", abs)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("sync path is not a directory: %s", abs)
	}
	return abs, nil
}

// discoveryWindow 单轮 UDP 扫描的收集窗口
const discoveryWindow = 2 * time.Second

// cliFlagsSet 返回本次命令行显式给出的旗子名集合（不含仅由 env 生效的默认值）
func cliFlagsSet() map[string]bool {
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// resolveSecret 落实密钥自管理（公网化支柱 C，docs/PUBLIC_EXPOSURE.md）。
// 解析优先级：显式 -k（含 env LOCAL_MIRROR_SECRET）＞ 密钥文件 ＞ 明文；
// --no-encrypt 强制明文（逃生门）。--show-key / --gen-key 是子命令式旗子：
// 前者打印后退出；后者生成后退出，除非还带了运行旗子（如 -m）才继续启动。
// key 只对 tty 输出，稳态横幅与日志只显指纹。
// 返回的错误属用法错误，调用方以退出码 2 处理
func resolveSecret() error {
	root := config.StartPath
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))

	if *config.ShowKey {
		if *config.GenKey {
			return fmt.Errorf("--show-key conflicts with --gen-key")
		}
		key, err := keyfile.Load(root)
		if err != nil {
			return err
		}
		if key == "" {
			fmt.Fprintf(os.Stderr, "local-mirror: no key file at %s (generate one with --gen-key)\n", keyfile.Path(root))
			os.Exit(1)
		}
		if !isTTY {
			return fmt.Errorf("refusing to print the key to a non-terminal (read %s directly if you must)", keyfile.Path(root))
		}
		fmt.Printf("key file:    %s\n", keyfile.Path(root))
		fmt.Printf("fingerprint: %s\n", keyfile.Fingerprint(key))
		fmt.Printf("key:         %s\n", key)
		os.Exit(0)
	}

	if *config.GenKey {
		// 一次只认一个密钥来源，避免"生成了 A、实际用的却是 B"
		if *config.Secret != "" {
			return fmt.Errorf("--gen-key conflicts with -k/--secret (or LOCAL_MIRROR_SECRET): pick one key source")
		}
		if *config.NoEncrypt {
			return fmt.Errorf("--gen-key conflicts with --no-encrypt")
		}
		key, err := keyfile.Generate(root, *config.Force)
		if err != nil {
			return err
		}
		fmt.Printf("generated key file: %s (mode 600)\n", keyfile.Path(root))
		fmt.Printf("fingerprint:        %s\n", keyfile.Fingerprint(key))
		if isTTY {
			fmt.Printf("key:                %s\n\n", key)
			fmt.Printf("on the dialing end (fill in this machine's address):\n")
			fmt.Printf("  local-mirror -m mirror -r <host> -p <dir> -k '%s'\n", key)
		} else {
			fmt.Printf("(key not shown: stdout is not a terminal; run --show-key in one)\n")
		}
		// 仅带 --gen-key（外加 --force / -p）＝ 像 wg genkey 一样生成即退出；
		// 带其他运行旗子才接着正常启动
		runFlags := cliFlagsSet()
		for _, name := range []string{"gen-key", "force", "path", "p"} {
			delete(runFlags, name)
		}
		if len(runFlags) == 0 {
			os.Exit(0)
		}
		*config.Secret = key
		config.SecretFromKeyFile = true
		fmt.Println()
		return nil
	}

	if *config.NoEncrypt {
		if *config.Secret != "" {
			return fmt.Errorf("--no-encrypt conflicts with -k/--secret (or LOCAL_MIRROR_SECRET)")
		}
		return nil
	}

	if *config.Secret != "" {
		// 显式最高优先（least surprise：文件优先会让 -k newvalue 被静默忽略）。
		// 拨号端对称持有：把 key 落进自己的密钥文件，下次启动可省 -k；
		// 内容一致时静默跳过，落盘失败不致命（本次仍按 -k 跑）
		if config.SyncsFromUpstream() {
			written, err := keyfile.Save(root, *config.Secret)
			if err != nil {
				log.Warnf("failed to save the key file (still running with -k): %v", err)
			} else if written {
				fmt.Printf("key saved to %s; -k can be omitted from now on\n", keyfile.Path(root))
			}
		}
		return nil
	}

	// 未给 -k：找密钥文件。文件在就自动开加密是往安全方向的 fail-safe，
	// 横幅显指纹可见不黑箱；文件也没有则保持明文（与从前一致）
	key, err := keyfile.Load(root)
	if err != nil {
		return err
	}
	if key != "" {
		*config.Secret = key
		config.SecretFromKeyFile = true
	}
	return nil
}

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
			fmt.Printf("scanning for LAN servers (%s)...\n", discoveryWindow)
		}
		servers, err := network.DiscoverServers(discoveryWindow, *config.Secret, config.InstanceID)
		if isTTY {
			// 扫描结束后擦掉进度行，选择列表（或横幅）原地出现，不留残余
			fmt.Print("\x1b[1A\x1b[K")
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "local-mirror: discovery failed: %v\nspecify the upstream server with -r\n", err)
			os.Exit(2)
		}

		if !isTTY {
			switch len(servers) {
			case 1:
				config.DiscoveredAddr = servers[0].Addr()
				config.DiscoveredAlias = servers[0].Alias
				log.Infof("discovered upstream: %s (%s)", servers[0].Addr(), servers[0].Alias)
				return
			case 0:
				fmt.Fprintf(os.Stderr, "local-mirror: no LAN server found (upstream not running yet? retry later), "+
					"or specify one with -r\n(discovery does not cross VPNs, subnets or firewalls)\n")
				os.Exit(1)
			default:
				fmt.Fprintf(os.Stderr, "local-mirror: found %d servers; cannot pick one non-interactively, use -r:\n", len(servers))
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
		idx, outcome, err := tui.Select(fmt.Sprintf("found %d local-mirror servers:", len(servers)), opts)
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
			fmt.Fprintf(os.Stderr, "local-mirror: other flags are ignored in --config mode: %v\n", extra)
			os.Exit(2)
		}
		multiCfg, err := config.LoadMultiConfig(*config.ConfigFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
			os.Exit(2)
		}
		if n := countRealityTasks(multiCfg); n > config.PortScanRange {
			fmt.Fprintf(os.Stderr, "local-mirror: warning: %d server tasks exceed the port scan range (%d); the excess cannot bind a port\n",
				n, config.PortScanRange)
		}
		runSupervisor(multiCfg) // 不返回
		return
	}

	// 用法错误：信息到 stderr，退出码 2（与 flag 包解析失败时的约定一致）
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "local-mirror: unknown arguments: %v\nsee --help for usage\n", flag.Args())
		os.Exit(2)
	}
	if _, ok := config.ModeMap[*config.Mode]; !ok {
		fmt.Fprintf(os.Stderr, "local-mirror: invalid mode %q (valid: reality, mirror, relay)\n", *config.Mode)
		os.Exit(2)
	}
	switch *config.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		fmt.Fprintf(os.Stderr, "local-mirror: invalid log level %q (valid: debug, info, warn, error)\n", *config.LogLevel)
		os.Exit(2)
	}
	root, err := resolveSyncRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
		os.Exit(2)
	}
	config.StartPath = root

	// 密钥自管理：解析出本次生效的口令（或处理 --gen-key/--show-key 后退出）。
	// 必须在发现流程、端口绑定、横幅之前——它们都消费 *config.Secret
	if err := resolveSecret(); err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
		os.Exit(2)
	}

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
				log.Errorf("error closing database: %v", err)
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
			log.Warnf("UDP discovery responder failed to start (clients can still use -r): %v", err)
		}
	}

	printBanner()
	log.Infof("startup: version=%s mode=%s instance=%08x root=%s", version, *config.Mode, config.InstanceID, config.StartPath)

	app.App()
}

// printBanner 向 stdout 输出启动状态。
// 长驻进程默认日志级别下终端不应完全静默，用户需要知道进程在做什么
// bannerFont 字标用的 3×5 像素点阵（M 为 5 像素宽、连字符 2 像素），
// 每字符一列串代表一行像素。渲染时两行像素折叠进一个字符格
// （▀ 上、▄ 下、█ 满），因此 5 行像素只占 3 行终端
var bannerFont = map[rune][]string{
	'L': {"100", "100", "100", "100", "111"},
	'O': {"111", "101", "101", "101", "111"},
	'C': {"111", "100", "100", "100", "111"},
	'A': {"111", "101", "111", "101", "101"},
	'M': {"10001", "11011", "10101", "10001", "10001"},
	'I': {"111", "010", "010", "010", "111"},
	'R': {"111", "101", "110", "101", "101"},
	'-': {"00", "00", "11", "00", "00"},
}

// renderWordmark 把单词渲染为 3 行半块字符画。字形间距 1 像素；
// "LOCAL-MIRROR" 全串恰好 48 像素宽，与横幅同宽
func renderWordmark(word string) []string {
	bitmap := make([]string, 5)
	for i, ch := range word {
		g, ok := bannerFont[ch]
		if !ok {
			continue
		}
		for r := range bitmap {
			// 连字符收紧左侧字距（side bearing）：字母统一 1 像素间距，
			// 但 '-' 笔画少且悬在中排，两侧都空一列会把词拆散；贴住左邻
			//（其右列中排本为空，不会粘笔画）、右侧保留 1 列呼吸
			if i > 0 && ch != '-' {
				bitmap[r] += "0"
			}
			bitmap[r] += g[r]
		}
	}
	bitmap = append(bitmap, strings.Repeat("0", len(bitmap[0]))) // 补齐偶数像素行
	out := make([]string, 0, 3)
	for r := 0; r < 6; r += 2 {
		var b strings.Builder
		for c := 0; c < len(bitmap[r]); c++ {
			up, down := bitmap[r][c] == '1', bitmap[r+1][c] == '1'
			switch {
			case up && down:
				b.WriteRune('█')
			case up:
				b.WriteRune('▀')
			case down:
				b.WriteRune('▄')
			default:
				b.WriteRune(' ')
			}
		}
		out = append(out, b.String())
	}
	return out
}

func printBanner() {
	p := termstyle.NewPalette(os.Stdout)
	const width = 48
	const labelWidth = 11

	line := p.Dim + strings.Repeat("─", width) + p.Reset
	row := func(label, value string) {
		pad := strings.Repeat(" ", max(1, labelWidth-termstyle.DisplayWidth(label)))
		fmt.Printf("  %s%s%s%s%s\n", p.Dim, label, p.Reset, pad, value)
	}

	modeDescMap := map[string]string{"reality": "server", "mirror": "client", "relay": "relay"}
	modeDesc := modeDescMap[*config.Mode]

	// 字标横幅：单行 "LOCAL-MIRROR"，实与虚用亮度表达——LOCAL 亮青、
	// MIRROR 压暗（这个字号下用 ░ 会糊，亮度对比才能保住字形）。
	// 前段 "LOCAL-" 与后段 "MIRROR" 分别渲染后逐行拼接，中间补一个
	// 字形间距像素列
	fmt.Println()
	solid := renderWordmark("LOCAL-")
	ghost := renderWordmark("MIRROR")
	for r := range solid {
		fmt.Printf("%s%s%s %s%s%s%s\n",
			p.Cyan, solid[r], p.Reset, p.Cyan, p.Dim, ghost[r], p.Reset)
	}
	fmt.Println()

	fmt.Println(line)
	fmt.Printf("  %s%sLocal Mirror%s %s  ·  %s%s%s (%s)\n",
		p.Bold, p.Cyan, p.Reset, version, p.Bold, *config.Mode, p.Reset, modeDesc)
	fmt.Println(line)
	row("Sync root", config.StartPath)
	// 忽略规则最多展示 4 条，其余折叠为计数（完整列表见 --help 与配置）
	ignoreShown := config.IgnoreFileList
	suffix := ""
	if len(ignoreShown) > 4 {
		suffix = fmt.Sprintf(" %s(+%d)%s", p.Dim, len(ignoreShown)-4, p.Reset)
		ignoreShown = ignoreShown[:4]
	}
	row("Ignores", strings.Join(ignoreShown, ", ")+suffix)
	if config.SyncsFromUpstream() {
		switch {
		case config.DiscoveredAddr != "":
			row("Upstream", fmt.Sprintf("%s%s%s %s(discovered: %s)%s",
				p.Green, config.DiscoveredAddr, p.Reset, p.Dim, config.DiscoveredAlias, p.Reset))
		default:
			ip := *config.RealityIP
			if ip == "" {
				ip = "127.0.0.1"
			}
			row("Upstream", fmt.Sprintf("%s%s%s %s(port scan %d-%d)%s",
				p.Green, ip, p.Reset, p.Dim, config.DefaultPort, config.DefaultPort+config.PortScanRange-1, p.Reset))
		}
	}
	if config.ServesDownstream() {
		if network.ListenedDualStack {
			row("Listen", fmt.Sprintf("%s:%d%s %s(IPv4 + IPv6)%s", p.Green, config.ActualPort, p.Reset, p.Dim, p.Reset))
		} else {
			row("Listen", fmt.Sprintf("%s0.0.0.0:%d%s %s(IPv4 only; host has no IPv6)%s", p.Green, config.ActualPort, p.Reset, p.Dim, p.Reset))
		}
	}
	switch {
	case *config.Secret != "" && config.SecretFromKeyFile:
		row("Encryption", fmt.Sprintf("%son%s (Noise NNpsk0, key file, fp %s)", p.Green, p.Reset, keyfile.Fingerprint(*config.Secret)))
	case *config.Secret != "":
		row("Encryption", fmt.Sprintf("%son%s (Noise NNpsk0, fp %s)", p.Green, p.Reset, keyfile.Fingerprint(*config.Secret)))
	case *config.NoEncrypt:
		row("Encryption", fmt.Sprintf("off %s(--no-encrypt: forced plaintext)%s", p.Dim, p.Reset))
	default:
		row("Encryption", fmt.Sprintf("off %s(plaintext; enable with -k or --gen-key)%s", p.Dim, p.Reset))
	}
	// 仅同步方（mirror/relay）涉及删除，展示当前删除策略
	if config.SyncsFromUpstream() {
		if *config.AllowDelete {
			row("Deletion", fmt.Sprintf("%son%s %s(faithful mirror; local extras get deleted)%s", p.Green, p.Reset, p.Dim, p.Reset))
		} else {
			row("Deletion", fmt.Sprintf("off %s(additive only; local extras kept)%s", p.Dim, p.Reset))
		}
		// 关键路径解锁档：提示覆盖前会快照备份
		if config.SnapshotOverwrites {
			row("Critical", fmt.Sprintf("%sunlocked%s %s(--allow-critical; first overwrite backed up to .local-mirror/backups)%s",
				p.Green, p.Reset, p.Dim, p.Reset))
		}
	}
	row("Instance", fmt.Sprintf("%08x", config.InstanceID))
	row("PID", fmt.Sprintf("%d", os.Getpid()))
	row("Log", fmt.Sprintf("%s %s(level %s)%s", logger.LogPath(), p.Dim, *config.LogLevel, p.Reset))
	fmt.Println(line)
}
