package main

import (
	"bufio"
	"flag"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/keyfile"
	"local-mirror/internal/network"
	"local-mirror/internal/tui"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/term"
)

// positionalArgs 命令行里的位置参数（位置写法 ./dir @host）
var positionalArgs []string

// parseInterspersed 让旗子可以写在位置参数之后（local-mirror ./dir @host -k key）：
// 标准 flag 包遇到第一个非旗子参数就停止解析，其后的 -k 会被当成未知参数。
// 逐个摘下位置参数、继续解析余下部分，直到参数耗尽
func parseInterspersed() []string {
	var pos []string
	for flag.NArg() > 0 {
		args := flag.Args()
		pos = append(pos, args[0])
		_ = flag.CommandLine.Parse(args[1:]) // ExitOnError：非法旗子自行报错退出
	}
	return pos
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

// resolveDirection 落实方向优先 CLI（公网化支柱 A）。
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

	if len(positionalArgs) > 0 {
		if modeGiven || upstreamGiven || dirVocab || set["p"] || set["path"] {
			return fmt.Errorf("positional SRC DST form cannot be mixed with -m/-r/-p or direction flags")
		}
		if len(positionalArgs) != 2 {
			return fmt.Errorf("unknown arguments: %v\npositional form: local-mirror ./dir @host[:port] (push) or local-mirror @host[:port] ./dir (pull)", positionalArgs)
		}
		a, b := positionalArgs[0], positionalArgs[1]
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

// resolveSecret 落实密钥自管理（公网化支柱 C）。
// 解析优先级：显式 -k（或内部的 --secret-stdin）＞ 密钥文件 ＞ 明文；
// --no-encrypt 强制明文（逃生门）。--show-key / --gen-key 是子命令式旗子：
// 前者打印后退出；后者生成后退出，除非还带了运行旗子（如 --send）才继续启动。
// key 只对 tty 输出，稳态横幅与日志只显指纹。
// 返回的错误属用法错误，调用方以退出码 2 处理
func resolveSecret() error {
	root := config.StartPath
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))

	// 监督进程传下来的口令走 stdin 首行（不进 argv、不进 environ）。
	// 归一成"显式 -k"后走下面完全相同的解析路径
	if *config.SecretStdin {
		if *config.Secret != "" {
			return fmt.Errorf("--secret-stdin conflicts with -k/--secret: pick one key source")
		}
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		// 父进程可能不带结尾换行就关闭管道：EOF 但已读到内容仍然有效
		if err != nil && line == "" {
			return fmt.Errorf("--secret-stdin: failed to read the key from stdin: %w", err)
		}
		key := strings.TrimRight(line, "\r\n")
		if key == "" {
			return fmt.Errorf("--secret-stdin: the key read from stdin is empty")
		}
		*config.Secret = key
	}

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
			return fmt.Errorf("--gen-key conflicts with -k/--secret: pick one key source")
		}
		if *config.NoEncrypt {
			return fmt.Errorf("--gen-key conflicts with --no-encrypt")
		}
		// 仅带 --gen-key（外加 --force / -p）＝ 像 wg genkey 一样生成即退出；
		// 带其他运行旗子才接着正常启动
		runFlags := cliFlagsSet()
		for _, name := range []string{"gen-key", "force", "path", "p"} {
			delete(runFlags, name)
		}
		starting := len(runFlags) > 0 || len(positionalArgs) > 0

		// 已有 key 且未 --force：沿用它。重启时照原命令（含 --gen-key）再跑一遍必须能起来；
		// 换 key 会让所有持旧 key 的拨号端断开，只在显式 --force 时做
		key, err := keyfile.Load(root)
		if err != nil {
			return err
		}
		switch {
		case key != "" && !*config.Force:
			fmt.Printf("key file exists, reusing it: %s (--force regenerates it and disconnects every dialer holding the old one)\n", keyfile.Path(root))
		case starting:
			// 要启动的新 key 推迟到拿到目录锁后落盘（persistPendingKey）：目录已被别的
			// 实例占用时本命令会失败，不能先把那个实例的 key 文件换掉
			if key, err = keyfile.NewKey(); err != nil {
				return err
			}
			keySavePending = true
			fmt.Printf("generated a new key, saved to %s (mode 600) once this instance starts\n", keyfile.Path(root))
		default:
			if key, err = keyfile.Generate(root, *config.Force); err != nil {
				return err
			}
			fmt.Printf("generated key file: %s (mode 600)\n", keyfile.Path(root))
		}
		fmt.Printf("fingerprint:        %s\n", keyfile.Fingerprint(key))
		if isTTY {
			fmt.Printf("key:                %s\n\n", key)
			label, cmd := peerKeyHint(key)
			fmt.Printf("%s\n  %s\n", label, cmd)
		} else {
			fmt.Printf("(key not shown: stdout is not a terminal; run --show-key in one)\n")
		}
		if !starting {
			os.Exit(0)
		}
		*config.Secret = key
		config.SecretFromKeyFile = true
		fmt.Println()
		return nil
	}

	if *config.NoEncrypt {
		if *config.Secret != "" {
			return fmt.Errorf("--no-encrypt conflicts with -k/--secret")
		}
		return nil
	}

	if *config.Secret != "" {
		// 显式最高优先（least surprise：文件优先会让 -k newvalue 被静默忽略）。
		// 汇端与拨出的源端对称持有：把 key 落进自己的密钥文件，下次启动可省 -k。
		// 落盘推迟到拿到目录锁之后（persistPendingKey）
		keySavePending = config.SyncsFromUpstream() || config.SourceDials
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

// peerKeyHint 推出对端配套的启动命令：数据方向取反、传输方向互补。
// 中继的 key 保护的是它自己的监听口，对端是拨进来的下游汇。
// 单独 --gen-key 未给方向时本端将来是源是汇都有可能，不猜，只提示带上同一个 key
func peerKeyHint(key string) (label, cmd string) {
	set := cliFlagsSet()
	if !*config.SendFlag && !*config.ReceiveFlag && !set["m"] && !set["mode"] {
		return "on the other end, pass the same key:", fmt.Sprintf("local-mirror ... -k '%s'", key)
	}
	peerDir := "--send"
	if config.ServesDownstream() {
		peerDir = "--receive"
	}
	if config.TransportListens() {
		return "on the dialing end (fill in this machine's address):",
			fmt.Sprintf("local-mirror %s --connect <host> -p <dir> -k '%s'", peerDir, key)
	}
	return "on the listening end:", fmt.Sprintf("local-mirror %s --listen -p <dir> -k '%s'", peerDir, key)
}

// keySavePending 本次生效的 key（-k 给的，或 --gen-key 新生成的）需要落进 key 文件，
// 由 persistPendingKey 在拿到目录锁后执行
var keySavePending bool

// persistPendingKey 把本次生效的 key 落进同步根的 key 文件。必须在 InitDB 拿到目录锁之后调用：
// 同一目录已有实例在跑时，新进程会在锁上失败退出，绝不能先把运行中实例的 key 文件
// 换成自己的（那样运行中实例下次重启就用错 key，所有对端都连不上）。
// 内容一致时静默跳过，落盘失败不致命（本次仍按 -k 跑）
func persistPendingKey() {
	if !keySavePending {
		return
	}
	written, err := keyfile.Save(config.StartPath, *config.Secret)
	if err != nil {
		log.Warnf("failed to save the key file (still running with -k): %v", err)
	} else if written {
		fmt.Printf("key saved to %s; -k can be omitted from now on\n", keyfile.Path(config.StartPath))
	}
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
