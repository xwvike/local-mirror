package main

import (
	"flag"
	"fmt"
	"local-mirror/config"
	app "local-mirror/internal"
	"local-mirror/internal/logger"
	"local-mirror/internal/network"
	"local-mirror/internal/safety"
	"local-mirror/internal/status"
	"local-mirror/internal/tree"
	"local-mirror/pkg/utils"
	"os"
	"runtime"
	"time"

	log "github.com/sirupsen/logrus"
)

// version 可在构建时注入: go build -ldflags "-X main.version=v1.2.3"
var version = "dev"

func init() {
	config.InstanceID = utils.GenerateRandomNum()
	config.StartTime = time.Now().Unix()
}

func main() {
	// 尽早切换控制台代码页：--help/--version 与用法错误的输出同样是中文。
	// os.Exit 的快速退出路径不经 defer，代码页会留在 UTF-8——两害相权：
	// 留下 65001 只影响极老的 GBK 输出程序，而不切换则本程序全部输出乱码
	restoreConsole := enableConsoleUTF8()
	defer restoreConsole()

	// 子命令分发必须在 flag.Parse() 之前，且只精确匹配 "service" 这一个词。
	// 不能用「argv[1] 不以 - 开头」来判定——位置糖 `local-mirror ./dir @peer`
	// 里的 ./dir 同样不以 - 开头，会被误当成子命令。
	// 代价是同步一个名为 service 的目录时要写 `-p ./service`，可接受
	if len(os.Args) > 1 && os.Args[1] == "service" {
		runServiceCommand(os.Args[2:]) // 不返回
	}

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
		// 单任务不 fork：监督进程存在的意义是管理多个子进程的生命周期，
		// 只有一个任务时那层父进程纯属开销（多一次调度、多一层信号转发，
		// 还让 pgrep/pkill 多一个匹配目标）。见 docs/CONFIG_AND_SERVICE.md §P3
		if len(multiCfg.Tasks) == 1 {
			applySingleTask(multiCfg.Tasks[0])
			// 落回下方单实例主流程，与命令行直接给旗子完全同路
		} else {
			if n := countRealityTasks(multiCfg); n > config.PortScanRange {
				fmt.Fprintf(os.Stderr, "local-mirror: warning: %d server tasks exceed the port scan range (%d); the excess cannot bind a port\n",
					n, config.PortScanRange)
			}
			runSupervisor(multiCfg) // 不返回
			return
		}
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
	// 数值旗子统一校验（CFG-01）：-f 0 会让发送循环空转、-c 0 会把低频安全网退化成
	// 每轮全量扫描。放在解析层而非监督层，直连 CLI/单任务/多任务子进程都覆盖到
	if err := config.ValidateRuntimeNumbers(); err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
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

	// SEC-01：监听端固定绑所有接口（无 --bind 选项），明文监听等于对任何网络可达者敞开
	// 文件服务（源监听）或汇的目录控制（汇监听）。故「明文 + 监听」要求用户显式确认：
	// 未设密钥（--gen-key / -k，或同步根里的密钥文件）又没显式 --no-encrypt 就拒绝启动，
	// 挡住「照无密钥公网示例直接暴露端口」。设了密钥的部署完全不受影响。
	if config.PlaintextListenBlocked() {
		fmt.Fprintf(os.Stderr, "local-mirror: refusing to listen in plaintext on all interfaces with no key. "+
			"Set a key (--gen-key on this listener, then -k on the dialer) for a private link, "+
			"or pass --no-encrypt to accept plaintext explicitly (only sane on a trusted LAN).\n")
		os.Exit(2)
	}
	// 显式选择明文监听：给一条醒目的启动告警（横幅里也有，但日志/journal 里要能一眼看到）
	if config.TransportListens() && *config.NoEncrypt {
		log.Warn("listening in PLAINTEXT on all interfaces (--no-encrypt): any network-reachable peer can read served files or drive this sink; use a key for anything beyond a trusted LAN")
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
