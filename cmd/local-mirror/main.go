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
	"local-mirror/internal/status"
	"local-mirror/internal/tree"
	"local-mirror/internal/tui"
	"local-mirror/internal/watcher"
	"local-mirror/pkg/termstyle"
	"local-mirror/pkg/utils"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
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

// resolveDirection 落实方向优先 CLI（公网化支柱 A，docs/PUBLIC_EXPOSURE.md §A.5）。
// 两个正交轴：数据方向 --send/--receive × 传输 --connect/--listen；位置糖
// `local-mirror ./dir @peer`（推）/ `local-mirror @peer ./dir`（拉）覆盖拨号常态。
// 解析结果落进既有内部状态（Mode/RealityIP）+ 两个新格（SourceDials/SinkListens）；
// -m/-r 保留为废弃别名原样生效，但不与新词汇混用（避免语义含糊）。
// 传输轴缺省即经典象限：--send 默认监听、--receive 默认拨出（含局域网发现）
func resolveDirection() error {
	set := cliFlagsSet()
	modeGiven := set["m"] || set["mode"]
	upstreamGiven := set["r"] || set["realityip"]
	dirVocab := set["send"] || set["receive"] || set["connect"] || set["listen"]

	if flag.NArg() > 0 {
		if modeGiven || upstreamGiven || dirVocab || set["p"] || set["path"] {
			return fmt.Errorf("positional SRC DST form cannot be mixed with -m/-r/-p or direction flags")
		}
		if flag.NArg() != 2 {
			return fmt.Errorf("unknown arguments: %v\npositional form: local-mirror ./dir @host[:port] (push) or local-mirror @host[:port] ./dir (pull)", flag.Args())
		}
		a, b := flag.Arg(0), flag.Arg(1)
		aRemote, bRemote := strings.HasPrefix(a, "@"), strings.HasPrefix(b, "@")
		switch {
		case aRemote == bRemote:
			return fmt.Errorf("positional form needs exactly one @peer and one local dir, got %q %q", a, b)
		case bRemote: // 本地在前 = 推：本端是源，拨向监听中的汇
			*config.SendFlag = true
			*config.ConnectTo = strings.TrimPrefix(b, "@")
			*config.Path = a
		default: // @ 在前 = 拉：本端是汇，拨向监听中的源（经典 mirror）
			*config.ReceiveFlag = true
			*config.ConnectTo = strings.TrimPrefix(a, "@")
			*config.Path = b
		}
		dirVocab = true
	}

	if !dirVocab {
		return nil // 老词汇：-m/-r 原样生效
	}
	if modeGiven || upstreamGiven {
		return fmt.Errorf("direction flags (--send/--receive/--connect/--listen) cannot be mixed with -m/-r: pick one vocabulary")
	}
	if !*config.SendFlag && !*config.ReceiveFlag {
		return fmt.Errorf("--connect/--listen need a direction: add --send (this dir is the source) or --receive (this dir is the sink)")
	}
	if *config.ConnectTo != "" && *config.ListenFlag {
		return fmt.Errorf("--connect and --listen are mutually exclusive on one link")
	}

	switch {
	case *config.SendFlag && *config.ReceiveFlag:
		// relay = 「向上 receive+connect ＋ 向下 send+listen」的组合，不是第三种模式
		*config.Mode = "relay"
		*config.RealityIP = *config.ConnectTo // 空则上游走局域网发现
	case *config.SendFlag:
		*config.Mode = "reality"
		if *config.ConnectTo != "" {
			// 新格「源拨出 → 汇监听」：本地维护、推向公网 VPS
			config.SourceDials = true
			*config.RealityIP = *config.ConnectTo
		}
	default:
		*config.Mode = "mirror"
		if *config.ListenFlag {
			// 新格「汇监听 ← 源拨入」
			config.SinkListens = true
		} else {
			*config.RealityIP = *config.ConnectTo // 空则局域网发现
		}
	}
	return nil
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
			fmt.Printf("  local-mirror --receive --connect <host> -p <dir> -k '%s'\n", key)
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
		// --status/--heat --config：聚合展示 YAML 里每个任务的观测数据（各读各自
		// 根下的 status.json / heat.json），而非启动监督进程。这是"通过 yml 部署
		// 了多台"的观测入口
		if *config.Status || *config.Heat {
			multiCfg, err := config.LoadMultiConfig(*config.ConfigFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
				os.Exit(2)
			}
			if *config.Heat {
				runHeatAggregate(multiCfg)
			} else {
				runStatusAggregate(multiCfg)
			}
			os.Exit(0)
		}
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

	// --status 与 --heat 都是只读观测子命令，语义不同，不能同时给
	if *config.Status && *config.Heat {
		fmt.Fprintf(os.Stderr, "local-mirror: --status and --heat are separate views; pass one at a time\n")
		os.Exit(2)
	}

	// --status/--heat --all：从进程表发现本机所有运行中的实例并聚合展示，不需要
	// 任何路径。放在方向/根解析之前——它与同步无关，也不占目录锁
	if *config.All {
		switch {
		case *config.Status:
			runStatusAll()
		case *config.Heat:
			runHeatAll()
		default:
			fmt.Fprintf(os.Stderr, "local-mirror: --all only applies together with --status or --heat\n")
			os.Exit(2)
		}
		os.Exit(0)
	}

	// 方向优先 CLI（公网化支柱 A）：位置糖与 --send/--receive × --connect/--listen
	// 两轴解析为内部状态；-m/-r 老词汇原样照跑。用法错误退出码 2
	if err := resolveDirection(); err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\nsee --help for usage\n", err)
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

	// --status：只读常驻进程写下的快照并渲染后退出。必须早于 InitDB——
	// 常驻进程持有目录锁，观测进程绝不能去抢锁。终端里进入实时刷新循环，
	// 管道/重定向时打印一次（脚本友好）
	if *config.Status {
		runStatusSingle(root)
		os.Exit(0)
	}

	// --heat：只读源侧常驻进程写下的 heat.json 并渲染目录热度表后退出。
	// 与 --status 同样早于 InitDB——绝不去抢常驻进程持有的目录锁
	if *config.Heat {
		runHeatSingle(root)
		os.Exit(0)
	}

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

	// 地址留空的拨出汇（mirror/relay 上游侧）先自动发现上游再继续启动。
	// 必须在 InitDB（单实例锁）之后：否则用户选完服务器才因目录被占退出。
	// 中继此刻自己的发现应答器尚未启动，结构上不会扫到自己。
	// 汇监听格不拨出、源拨出格必带地址（resolveDirection 已校验），都不发现
	if config.SyncsFromUpstream() && !config.SinkListens && *config.RealityIP == "" {
		runDiscovery()
	}

	// 监听的一方（源监听 = 经典 reality/relay 下游，或汇监听格）在打印横幅前
	// 先绑定端口（从 DefaultPort 起自动探测），横幅里展示的才是真实监听端口；
	// accept 循环稍后由 Reality / MirrorListen 启动
	if config.TransportListens() {
		listener, port, err := network.ListenAvailable(config.DefaultPort, config.PortScanRange)
		if err != nil {
			fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
			os.Exit(1)
		}
		app.ServerListener = listener
		config.ActualPort = port

		// UDP 发现应答器只属于监听中的源（局域网 mirror 找 reality 的机制）；
		// 监听中的汇不是源，不应答发现。失败不致命（客户端仍可 -r 直连）
		if config.ServesDownstream() {
			if _, err := network.StartDiscoveryResponder(port, config.AliasName, config.StartPath, *config.Secret); err != nil {
				log.Warnf("UDP discovery responder failed to start (clients can still use -r): %v", err)
			}
		}
	}

	printBanner()
	log.Infof("startup: version=%s mode=%s instance=%08x root=%s", version, *config.Mode, config.InstanceID, config.StartPath)

	// 运维快照：定型 identity 段并启动后台落盘循环，供 --status 读取。
	// 落进 .local-mirror/status.json（可弃状态，删了下次自建）
	status.Init(config.StartPath, version, fmt.Sprintf("%08x", config.InstanceID),
		directionLabel(), transportLabel(), peerLabel(), *config.Secret != "", config.StartTime)
	stopStatus := make(chan struct{})
	go status.Run(stopStatus)

	app.App()
	close(stopStatus) // 收到退出信号后停止落盘（App 返回即已收到 SIGINT/SIGTERM）
}

// directionLabel/transportLabel/peerLabel 供 status 与人读展示：把内部 mode +
// 四象限状态翻译成方向优先的词汇
func directionLabel() string {
	switch *config.Mode {
	case "reality":
		return "send · source"
	case "mirror":
		return "receive · sink"
	case "relay":
		return "relay"
	}
	return *config.Mode
}

func transportLabel() string {
	if config.TransportListens() {
		return "listen"
	}
	return "dial"
}

func peerLabel() string {
	if config.TransportListens() {
		return "inbound"
	}
	if config.DiscoveredAddr != "" {
		return config.DiscoveredAddr
	}
	host, port := network.SplitPeer(*config.RealityIP)
	if host == "" {
		return "(LAN discovery)"
	}
	if port == 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
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

// humanBytes 与 humanDuration 供 --status 展示
func humanStatusBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func humanSince(t time.Time) string {
	if t.IsZero() || t.Unix() == 0 {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm ago", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh ago", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func humanUptime(started int64) string {
	if started == 0 {
		return "?"
	}
	d := time.Since(time.Unix(started, 0))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func humanRate(bps float64) string {
	switch {
	case bps >= 1<<20:
		return fmt.Sprintf("%.1f MB/s", bps/(1<<20))
	case bps >= 1<<10:
		return fmt.Sprintf("%.0f KB/s", bps/(1<<10))
	case bps > 0:
		return fmt.Sprintf("%.0f B/s", bps)
	default:
		return "—"
	}
}

// progressBar 渲染定宽进度条 [████░░░░] 66%
func progressBar(done, total uint64, width int, p termstyle.Palette) string {
	if total == 0 {
		return p.Dim + strings.Repeat("░", width) + p.Reset + "   —"
	}
	frac := float64(done) / float64(total)
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * float64(width))
	return fmt.Sprintf("%s%s%s%s%s %3d%%",
		p.Green, strings.Repeat("█", filled), p.Dim, strings.Repeat("░", width-filled), p.Reset, int(frac*100))
}

func fileSuffix(name string, p termstyle.Palette) string {
	if name == "" {
		return ""
	}
	return fmt.Sprintf("  %s(%s)%s", p.Dim, name, p.Reset)
}

// padCell 按显示宽度右填充到 w 列（ANSI 色码会破坏 %-Ns 对齐，故先按纯文本
// 计宽再上色）
func padCell(text string, w int) string {
	if dw := termstyle.DisplayWidth(text); dw < w {
		return text + strings.Repeat(" ", w-dw)
	}
	return text
}

// runStatusSingle 单实例 --status：终端进实时刷新循环，管道则打印一次
func runStatusSingle(root string) {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		liveLoop(func() { renderSingle(root) })
	} else {
		renderSingle(root)
	}
}

// runStatusAggregate 多实例 --status --config：聚合 YAML 每个任务的状态
func runStatusAggregate(cfg *config.MultiConfig) {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		liveLoop(func() { renderAggregate(cfg) })
	} else {
		renderAggregate(cfg)
	}
}

// liveLoop 实时刷新：备用屏 + 隐藏光标，每秒重绘一帧，Ctrl-C 退出并还原终端。
// 走 os.Exit 会跳过 defer，故进出终端态都显式做，不依赖 defer
func liveLoop(frame func()) {
	p := termstyle.NewPalette(os.Stdout)
	fmt.Print("\033[?1049h\033[?25l") // 备用屏 + 隐藏光标
	leave := func() { fmt.Print("\033[?25h\033[?1049l") }
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	t := time.NewTicker(1 * time.Second)
	for {
		fmt.Print("\033[H\033[2J") // 光标归位 + 清屏
		frame()
		fmt.Printf("\n  %srefresh 1s · Ctrl-C to exit%s\n", p.Dim, p.Reset)
		select {
		case <-sig:
			t.Stop()
			leave()
			return
		case <-t.C:
		}
	}
}

// renderSingle 渲染单个实例的运行时快照（每帧重新读盘）。
// 无快照文件 = 没有实例在此根跑过；快照陈旧 = 进程可能已停
func renderSingle(root string) {
	p := termstyle.NewPalette(os.Stdout)
	snap, err := status.Load(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: cannot read status: %v\n", err)
		return
	}
	if snap == nil {
		fmt.Printf("\n  %sno status for%s %s\n", p.Dim, p.Reset, root)
		fmt.Printf("  %s(no instance has run here, or .local-mirror/status.json was removed)%s\n", p.Dim, p.Reset)
		return
	}

	const width = 54
	const labelWidth = 12
	line := p.Dim + strings.Repeat("─", width) + p.Reset
	row := func(label, value string) {
		pad := strings.Repeat(" ", max(1, labelWidth-termstyle.DisplayWidth(label)))
		fmt.Printf("  %s%s%s%s%s\n", p.Dim, label, p.Reset, pad, value)
	}

	// 存活判据：陈旧快照说明落盘循环停了（进程多半已退）——替代 ps 的那一眼
	live := !snap.Stale()
	fmt.Println()
	fmt.Println(line)
	if live {
		fmt.Printf("  %s%sStatus%s      %s%s● running%s   %spid %d · up %s%s\n",
			p.Bold, p.Cyan, p.Reset, p.Bold, p.Green, p.Reset, p.Dim, snap.PID, humanUptime(snap.StartedUnix), p.Reset)
	} else {
		fmt.Printf("  %s%sStatus%s      %s○ stale%s   %slast update %s · pid %d may be dead%s\n",
			p.Bold, p.Cyan, p.Reset, p.Yellow, p.Reset, p.Dim, humanSince(time.Unix(snap.UpdatedUnix, 0)), snap.PID, p.Reset)
	}
	fmt.Println(line)

	row("Direction", fmt.Sprintf("%s   %s(%s)%s", snap.Direction, p.Dim, snap.Transport, p.Reset))
	row("Peer", snap.Peer)
	switch {
	case !live && snap.Connected:
		// 进程已停：连接字段是死前的最后已知态，别用绿色误导成"此刻在连"
		row("Link", fmt.Sprintf("%s○ %s (last known)%s", p.Dim, snap.Detail, p.Reset))
	case snap.Connected:
		row("Link", fmt.Sprintf("%s● %s%s", p.Green, snap.Detail, p.Reset))
	default:
		detail := "idle (no active connection)"
		if snap.Detail != "" {
			detail = snap.Detail
		}
		row("Link", fmt.Sprintf("%s○ %s%s", p.Dim, detail, p.Reset))
	}
	enc := "off (plaintext)"
	if snap.Encrypted {
		enc = "on (Noise NNpsk0)"
	}
	row("Encryption", enc)
	row("Sync root", snap.Root)
	fmt.Println(line)

	// 传输段：进行中的文件带进度条 + 速率；空闲则只显速率/idle
	if live && snap.CurrentFile != "" {
		row("Transfer", fmt.Sprintf("%s▶%s %s", p.Cyan, p.Reset, snap.CurrentFile))
		row("", fmt.Sprintf("%s   %s / %s   %s%s%s",
			progressBar(snap.CurrentDone, snap.CurrentTotal, 20, p),
			humanStatusBytes(snap.CurrentDone), humanStatusBytes(snap.CurrentTotal),
			p.Bold, humanRate(snap.RateBps), p.Reset))
	} else {
		state := "idle"
		if live && snap.RateBps > 0 {
			state = humanRate(snap.RateBps)
		}
		row("Transfer", fmt.Sprintf("%s%s%s", p.Dim, state, p.Reset))
	}
	row("Totals", fmt.Sprintf("%s / %d files   %s· last %s%s%s",
		humanStatusBytes(snap.Bytes), snap.Files, p.Dim, humanSince(time.Unix(snap.LastSyncUnix, 0)), fileSuffix(snap.LastFile, p), p.Reset))
	if snap.Errors > 0 {
		row("Errors", fmt.Sprintf("%s%d%s", p.Yellow, snap.Errors, p.Reset))
	} else {
		row("Errors", "0")
	}
	fmt.Println(line)

	// 资源段（常驻进程自采）
	row("CPU", fmt.Sprintf("%.1f%%", snap.CPUPercent))
	row("Memory", memoryLine(snap))
	row("FDs", fdLine(snap))
	row("Goroutines", fmt.Sprintf("%d", snap.Goroutines))
	fmt.Println(line)
}

// memoryLine 组装内存展示：有 OS RSS 就以它为主，附 Go 堆/申请量
func memoryLine(s *status.Snapshot) string {
	heap := fmt.Sprintf("%s heap · %s sys", humanStatusBytes(s.HeapBytes), humanStatusBytes(s.SysBytes))
	if s.HasRSS {
		return fmt.Sprintf("%s rss   (%s)", humanStatusBytes(s.RSSBytes), heap)
	}
	return heap
}

func fdLine(s *status.Snapshot) string {
	if s.HasFDs {
		return fmt.Sprintf("%d", s.FDs)
	}
	return "— (not available on this platform)"
}

func dirShort(mode string) string {
	switch mode {
	case "reality":
		return "send"
	case "mirror":
		return "recv"
	case "relay":
		return "relay"
	}
	return mode
}

// dirShortFromSnap 从快照的方向字串（"send · source" 等）取短标签，
// 供 --all 使用（发现来的实例没有原始 mode，只有已渲染的方向串）
func dirShortFromSnap(s *status.Snapshot) string {
	switch {
	case strings.HasPrefix(s.Direction, "send"):
		return "send"
	case strings.HasPrefix(s.Direction, "receive"):
		return "recv"
	case strings.HasPrefix(s.Direction, "relay"):
		return "relay"
	}
	return s.Direction
}

// statusRow 聚合表的一行。Snap 为 nil 表示该行对应的实例未启动
type statusRow struct {
	Name string
	Dir  string
	Snap *status.Snapshot
}

// renderStatusTable 渲染聚合表：每实例一行，列对齐（色码不计入列宽，见 padCell）。
// --config（YAML 多任务）与 --all（进程表发现）共用
func renderStatusTable(rows []statusRow, p termstyle.Palette) {
	fmt.Printf("  %s%s %s %s %s %s %s %s %s%s\n", p.Dim,
		padCell("NAME", 16), padCell("DIR", 6), padCell("LINK", 5),
		padCell("RATE", 11), padCell("FILES", 7), padCell("LAST", 9),
		padCell("CPU", 6), padCell("MEM", 10), p.Reset)

	for _, r := range rows {
		snap := r.Snap
		rate, files, last, cpu, mem := "—", "—", "—", "—", "—"
		var link string
		switch {
		case snap == nil:
			link = p.Dim + padCell("—", 5) + p.Reset
		case snap.Stale():
			link = p.Yellow + padCell("○", 5) + p.Reset
		case snap.Connected:
			link = p.Green + padCell("●", 5) + p.Reset
		default:
			link = p.Dim + padCell("○", 5) + p.Reset
		}
		if snap != nil {
			if snap.RateBps > 0 {
				rate = humanRate(snap.RateBps)
			}
			files = fmt.Sprintf("%d", snap.Files)
			last = humanSince(time.Unix(snap.LastSyncUnix, 0))
			cpu = fmt.Sprintf("%.1f%%", snap.CPUPercent)
			if snap.HasRSS {
				mem = humanStatusBytes(snap.RSSBytes)
			} else {
				mem = humanStatusBytes(snap.HeapBytes)
			}
		}
		suffix := ""
		if snap != nil && snap.Stale() {
			suffix = p.Yellow + "  (stale)" + p.Reset
		} else if snap == nil {
			suffix = p.Dim + "  (not started)" + p.Reset
		}
		fmt.Printf("  %s %s %s %s %s %s %s %s%s\n",
			padCell(termstyle.Truncate(r.Name, 16), 16), padCell(r.Dir, 6), link,
			padCell(rate, 11), padCell(files, 7), padCell(last, 9),
			padCell(cpu, 6), padCell(mem, 10), suffix)
	}
}

// renderAggregate 渲染 YAML 多任务的聚合表：每任务一行，各读各自根下的快照
func renderAggregate(cfg *config.MultiConfig) {
	p := termstyle.NewPalette(os.Stdout)
	fmt.Println()
	fmt.Printf("  %s%slocal-mirror%s   %s%d tasks%s\n", p.Bold, p.Cyan, p.Reset, p.Dim, len(cfg.Tasks), p.Reset)
	fmt.Println()
	rows := make([]statusRow, 0, len(cfg.Tasks))
	for i := range cfg.Tasks {
		t := cfg.Tasks[i]
		snap, _ := status.Load(t.Path)
		rows = append(rows, statusRow{Name: t.Name, Dir: dirShort(t.Mode), Snap: snap})
	}
	renderStatusTable(rows, p)
}

// runStatusAll 全机发现视图：终端进实时刷新循环，管道则打印一次
func runStatusAll() {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		liveLoop(renderAll)
	} else {
		renderAll()
	}
}

// renderAll 从进程表发现本机所有运行中的实例并聚合展示（每帧重新发现）
func renderAll() {
	p := termstyle.NewPalette(os.Stdout)
	instances := status.DiscoverInstances()
	fmt.Println()
	fmt.Printf("  %s%slocal-mirror%s   %s%d running on this host%s\n",
		p.Bold, p.Cyan, p.Reset, p.Dim, len(instances), p.Reset)
	if len(instances) == 0 {
		fmt.Printf("\n  %sno running local-mirror instances found%s\n", p.Dim, p.Reset)
		fmt.Printf("  %s(--all scans the process table for daemons that write .local-mirror/status.json;\n", p.Dim)
		fmt.Printf("   pre-status builds won't appear)%s\n", p.Reset)
		return
	}
	fmt.Println()
	rows := make([]statusRow, 0, len(instances))
	for _, inst := range instances {
		rows = append(rows, statusRow{Name: shortRoot(inst.Root), Dir: dirShortFromSnap(inst.Snap), Snap: inst.Snap})
	}
	renderStatusTable(rows, p)
}

// heatMaxRows 终端里单个热度表最多展示的行数；其余（都是低分 tier2）折叠为计数。
// 观测关心的是"我干活的目录有没有拿到实时 watch"，高分在前已足够
const heatMaxRows = 40

// runHeatSingle 单实例 --heat：终端进实时刷新循环，管道则打印一次
func runHeatSingle(root string) {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		liveLoop(func() { renderHeatSingle(root) })
	} else {
		renderHeatSingle(root)
	}
}

// runHeatAll 全机 --heat --all：发现本机所有源实例并逐个展示热度表
func runHeatAll() {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		liveLoop(renderHeatAll)
	} else {
		renderHeatAll()
	}
}

// runHeatAggregate 多实例 --heat --config：聚合 YAML 每个任务的热度表
func runHeatAggregate(cfg *config.MultiConfig) {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		liveLoop(func() { renderHeatAggregate(cfg) })
	} else {
		renderHeatAggregate(cfg)
	}
}

// renderHeatSingle 渲染单个同步根的目录热度表（每帧重新读盘）
func renderHeatSingle(root string) {
	p := termstyle.NewPalette(os.Stdout)
	snap, err := watcher.LoadHeat(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: cannot read heat table: %v\n", err)
		return
	}
	fmt.Println()
	if snap == nil {
		fmt.Printf("  %sno heat table for%s %s\n", p.Dim, p.Reset, root)
		fmt.Printf("  %s(only a running source or relay builds one; a sink has none)%s\n", p.Dim, p.Reset)
		return
	}
	fmt.Printf("  %s%sheat%s   %s%s%s\n", p.Bold, p.Cyan, p.Reset, p.Dim, root, p.Reset)
	renderHeatTable(snap, p)
}

// renderHeatAll 从进程表发现本机所有实例，逐个展示各自的热度表（源才有）
func renderHeatAll() {
	p := termstyle.NewPalette(os.Stdout)
	instances := status.DiscoverInstances()
	fmt.Println()
	fmt.Printf("  %s%slocal-mirror heat%s   %s%d running on this host%s\n",
		p.Bold, p.Cyan, p.Reset, p.Dim, len(instances), p.Reset)
	shown := 0
	for _, inst := range instances {
		snap, err := watcher.LoadHeat(inst.Root)
		if err != nil || snap == nil {
			continue // 汇实例无热度表，跳过
		}
		fmt.Printf("\n  %s%s%s\n", p.Bold, shortRoot(inst.Root), p.Reset)
		renderHeatTable(snap, p)
		shown++
	}
	if shown == 0 {
		fmt.Printf("\n  %sno source with a heat table found (sinks don't build one)%s\n", p.Dim, p.Reset)
	}
}

// renderHeatAggregate 逐个展示 YAML 每个任务的热度表（各读各自根下的 heat.json）
func renderHeatAggregate(cfg *config.MultiConfig) {
	p := termstyle.NewPalette(os.Stdout)
	fmt.Println()
	fmt.Printf("  %s%slocal-mirror heat%s   %s%d tasks%s\n", p.Bold, p.Cyan, p.Reset, p.Dim, len(cfg.Tasks), p.Reset)
	shown := 0
	for i := range cfg.Tasks {
		t := cfg.Tasks[i]
		snap, err := watcher.LoadHeat(t.Path)
		if err != nil || snap == nil {
			continue
		}
		fmt.Printf("\n  %s%s%s\n", p.Bold, t.Name, p.Reset)
		renderHeatTable(snap, p)
		shown++
	}
	if shown == 0 {
		fmt.Printf("\n  %sno task with a heat table found (only source/relay tasks build one)%s\n", p.Dim, p.Reset)
	}
}

// renderHeatTable 热度表主体：分数降序，tier1（实时 watch）绿标，超出 heatMaxRows
// 的低分尾部折叠为计数
func renderHeatTable(snap *watcher.HeatSnapshot, p termstyle.Palette) {
	stale := ""
	if snap.Stale() {
		stale = p.Yellow + "   (stale: source may have stopped)" + p.Reset
	}
	fmt.Printf("  %stier1 (real-time watch) %d/%d · tier2 (lazy poll) %d · %d dirs%s%s\n",
		p.Dim, snap.Tier1Count, snap.Tier1Limit, snap.Total-snap.Tier1Count, snap.Total, p.Reset, stale)
	if snap.Total == 0 {
		fmt.Printf("  %s(no directories scored yet)%s\n", p.Dim, p.Reset)
		return
	}
	fmt.Printf("  %s%s %s %s %s%s\n", p.Dim,
		padCell("SCORE", 9), padCell("TIER", 6), padCell("EVENTS", 8), "DIRECTORY", p.Reset)
	for i, e := range snap.Entries {
		if i >= heatMaxRows {
			fmt.Printf("  %s… +%d more (tier2, lower score)%s\n", p.Dim, len(snap.Entries)-heatMaxRows, p.Reset)
			break
		}
		tier, tcol := "tier2", p.Dim
		if e.Tier == 1 {
			tier, tcol = "tier1", p.Green
		}
		dir := e.Path
		if dir == "" || dir == "." {
			dir = ". (sync root)"
		}
		fmt.Printf("  %s %s%s%s %s %s\n",
			padCell(fmt.Sprintf("%.2f", e.Score), 9),
			tcol, padCell(tier, 6), p.Reset,
			padCell(fmt.Sprintf("%d", e.Events), 8), dir)
	}
}

// shortRoot 取同步根的末两段做行标签（如 proj/src），比单纯 basename 更能
// 区分"多个根同名 basename"（backup/src 与 proj/src）
func shortRoot(root string) string {
	base := filepath.Base(root)
	parent := filepath.Base(filepath.Dir(root))
	if parent == "." || parent == "/" || parent == "" {
		return base
	}
	return parent + "/" + base
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

	// 方向优先：横幅用 send/receive 说话，-m 老词汇只是别名
	modeDescMap := map[string]string{"reality": "send · source", "mirror": "receive · sink", "relay": "relay"}
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
	if config.SyncsFromUpstream() && !config.SinkListens {
		switch {
		case config.DiscoveredAddr != "":
			row("Upstream", fmt.Sprintf("%s%s%s %s(discovered: %s)%s",
				p.Green, config.DiscoveredAddr, p.Reset, p.Dim, config.DiscoveredAlias, p.Reset))
		default:
			host, port := network.SplitPeer(*config.RealityIP)
			if host == "" {
				host = "127.0.0.1"
			}
			if port != 0 {
				row("Upstream", fmt.Sprintf("%s%s%s %s(pinned port)%s",
					p.Green, net.JoinHostPort(host, strconv.Itoa(port)), p.Reset, p.Dim, p.Reset))
			} else {
				row("Upstream", fmt.Sprintf("%s%s%s %s(port scan %d-%d)%s",
					p.Green, host, p.Reset, p.Dim, config.DefaultPort, config.DefaultPort+config.PortScanRange-1, p.Reset))
			}
		}
	}
	// 汇监听格：上游没有地址可显，等源拨入
	if config.SinkListens {
		row("Source", fmt.Sprintf("inbound %s(waiting for the source to dial us)%s", p.Dim, p.Reset))
	}
	// 源拨出格：对端是监听中的汇
	if config.SourceDials {
		host, port := network.SplitPeer(*config.RealityIP)
		if port == 0 {
			port = config.DefaultPort
		}
		row("Sink", fmt.Sprintf("%s%s%s %s(dialing out; the sink listens)%s",
			p.Green, net.JoinHostPort(host, strconv.Itoa(port)), p.Reset, p.Dim, p.Reset))
	}
	// 监听行属于任何监听的一方：经典源、relay 下游、以及汇监听格
	if config.TransportListens() {
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
	// 提示观测命令：--status 是独立只读进程，不惊动本服务
	fmt.Printf("  %swatch:  local-mirror --status -p %s   %s(or --status --all)%s\n",
		p.Dim, config.StartPath, p.Dim, p.Reset)
	fmt.Println()
}
