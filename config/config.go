package config

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// 运行模式（v2 起 CLI 层溶解为「方向」，此处保留为内部状态与老值映射）
	RealityMode = 0x0001
	MirrorMode  = 0x0002
	RelayMode   = 0x0003

	// 握手 Role 字段承载的数据方向（公网化支柱 A）：老值平滑映射——
	// reality 一直发 1、mirror 一直发 2，语义由「模式」重释为「方向」；
	// RelayMode(3) 是旧 relay 两个方向都发的遗留值，握手校验里视为合法
	RoleSend    uint8 = RealityMode // 本端是源（数据流出）
	RoleReceive uint8 = MirrorMode  // 本端是汇（数据流入）

	// DefaultPort 端口探测的起始 TCP 端口。
	// 服务端从这里开始寻找第一个可用端口监听；
	// 客户端在 [DefaultPort, DefaultPort+PortScanRange) 范围内逐个握手探测服务端
	DefaultPort = 52345
	// PortScanRange 端口探测范围大小
	PortScanRange = 10
)

var (
	// forcedIgnores 强制忽略项：任何情况下都不同步，不可用 ! 取消（不变量）。
	// .local-mirror 是状态目录（cache.db/status.json/observe/key…），同步它=自指
	forcedIgnores = []string{".local-mirror"}
	// defaultIgnores 默认忽略项：合理的开箱默认，但属"政策"非"不变量"，
	// 可用 -i '!pattern'（或 ignore 文件里的 !pattern）逐项取消。
	// .git 是版本控制数据库（用文件复制器实时抄会抓到不一致态，该交给 git 自己
	// 复制）、.DS_Store 是 macOS 元数据垃圾，默认都不该同步
	defaultIgnores = []string{".git", ".DS_Store"}

	// IgnoreFileList 生效的忽略列表：forced + default（去掉被 ! 取消的）+ -i/--ignore
	// + .local-mirror/ignore 文件，合并去重后的结果（见 LoadIgnoreList，启动时调用
	// 一次，不热加载）。匹配按路径段进行，每段支持 * ? [] 通配符（见 utils.IsIgnored）。
	// 服务端命中即不扫描/不监听（不进树），客户端命中即不同步（不下载也不删除）。
	// 未调用 LoadIgnoreList 前即 forced + default
	IgnoreFileList = append(append([]string{}, forcedIgnores...), defaultIgnores...)
)

var (
	ModeMap = map[string]uint8{
		"reality": RealityMode,
		"mirror":  MirrorMode,
		"relay":   RelayMode,
	}
)

var (
	Mode           *string
	LogLevel       *string
	CoolDown       *int64
	FileBufferSize *uint64
	RealityIP      *string
	Secret         *string
	Path           *string
	Alias          *string
	Ignore         *string
	ConfigFile     *string
	AllowDelete    *bool
	AllowCritical  *bool
	GenKey         *bool
	ShowKey        *bool
	NoEncrypt      *bool
	Force          *bool
	Status         *bool
	All            *bool
	Heat           *bool
	SendFlag       *bool
	ReceiveFlag    *bool
	ConnectTo      *string
	ListenFlag     *bool
	Help           *bool
	Version        *bool

	// 四象限（公网化支柱 A）：数据方向（send/receive，内部沿用 Mode 表达）
	// 与连接方向（listen/dial）解耦后的两个新格。由 main.resolveDirection
	// 依 --send/--receive × --connect/--listen 推导：
	// SourceDials = 源端拨出（--send --connect，不监听、拨向监听的汇）；
	// SinkListens = 汇端监听（--receive --listen，不拨出、等源拨入）
	SourceDials bool   = false
	SinkListens bool   = false
	ActualPort  int    = 0          // 服务端实际监听的端口（启动时探测确定）
	StartPath   string = ""         // 同步根目录（-p 指定，默认为当前工作目录）
	InstanceID  uint32 = 0x00000000 // Instance ID
	// ProtocolVersion 本端支持的最高协议版本。
	// v2：变更查询改为长轮询推送；v3：握手可协商化（区间+能力位）、
	// 结构化错误、树响应分页、变更超限降级、清理死消息类型
	ProtocolVersion uint16 = 0x0003
	// MinProtocolVersion 本端支持的最低协议版本。两端在
	// [Min, Version] 区间交集内取最高值为会话版本（见 network 协议约定）；
	// 交集为空则握手拒绝。当前两值相等，行为与严格相等一致
	MinProtocolVersion uint16 = 0x0003
	StartTime          int64  = 0 // Start time

	// AliasName 解析后的最终实例别名（--alias → 主机名 → "local-mirror"），
	// 服务端在 UDP 发现应答中广播，供客户端选择列表展示
	AliasName string = ""
	// DiscoveredAddr/DiscoveredAlias 客户端自动发现选定的上游 "ip:port" 与其别名。
	// 仅在 -r 留空且发现流程成功时非空；InitConn 优先直连该地址
	DiscoveredAddr  string = ""
	DiscoveredAlias string = ""

	// SnapshotOverwrites 为真时，客户端覆盖已有文件前先把原文件快照到
	// .local-mirror/backups（关键路径 + --allow-critical 档位）。启动时置位
	SnapshotOverwrites bool = false

	// SecretFromKeyFile 生效口令来自 .local-mirror/key 密钥文件而非显式 -k
	//（横幅展示来源用）。密钥解析优先级见 main.resolveSecret：
	// 显式 -k（含 env）＞ 密钥文件 ＞ 明文；--no-encrypt 强制明文
	SecretFromKeyFile bool = false
)

// LoadIgnoreList 合并生效忽略列表：forcedIgnores（强制，不可取消）+ defaultIgnores
// （默认，可用 !pattern 逐项取消）+ -i/--ignore 逗号分隔条目 + <startPath>/.local-mirror/ignore
// 文件（每行一条，# 注释，空行跳过，文件不存在则静默跳过）。以 ! 开头的条目表示
// "取消一个默认忽略项"（如 !.git 让 .git 参与同步）；! 不能取消强制项。普通模式用
// filepath.Match 预校验（非法如未闭合的 "[" 返回错误）。结果去重（保序）后写回
// IgnoreFileList。启动时调用一次；运行中修改文件不生效，需重启
func LoadIgnoreList(startPath string) error {
	var adds []string                // -i/文件里的普通忽略模式（叠加）
	negated := make(map[string]bool) // 被 !pattern 取消的默认项

	collect := func(raw string) error {
		p := strings.TrimSpace(raw)
		if p == "" || strings.HasPrefix(p, "#") {
			return nil
		}
		if neg, ok := strings.CutPrefix(p, "!"); ok {
			if neg = strings.TrimSpace(neg); neg == "" {
				return nil
			}
			for _, f := range forcedIgnores {
				if neg == f {
					return fmt.Errorf("cannot un-ignore %q: it is a forced ignore (the state dir must never be synced)", neg)
				}
			}
			negated[neg] = true // 非默认项的 ! 属无操作（该项本就不在列表里）
			return nil
		}
		if _, err := filepath.Match(p, "x"); err != nil {
			return fmt.Errorf("invalid ignore pattern %q: %w", p, err)
		}
		adds = append(adds, p)
		return nil
	}

	if *Ignore != "" {
		for _, p := range strings.Split(*Ignore, ",") {
			if err := collect(p); err != nil {
				return err
			}
		}
	}

	ignoreFile := filepath.Join(startPath, ".local-mirror", "ignore")
	if data, err := os.ReadFile(ignoreFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if err := collect(line); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to read ignore file %s: %w", ignoreFile, err)
	}

	// 组装：强制项恒在；默认项去掉被取消的；再叠加 -i/文件的普通条目
	patterns := append([]string{}, forcedIgnores...)
	for _, d := range defaultIgnores {
		if !negated[d] {
			patterns = append(patterns, d)
		}
	}
	patterns = append(patterns, adds...)

	seen := make(map[string]struct{}, len(patterns))
	merged := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		merged = append(merged, p)
	}
	IgnoreFileList = merged
	return nil
}

// ServesDownstream 本进程是否运行源引擎（对外送数据）。
// 注意这只是数据方向：传输上源可能监听也可能拨出（见 TransportListens）
func ServesDownstream() bool {
	return *Mode == "reality" || *Mode == "relay"
}

// SyncsFromUpstream 本进程是否运行汇引擎（收数据）。
// 同上，汇可能拨出也可能监听
func SyncsFromUpstream() bool {
	return *Mode == "mirror" || *Mode == "relay"
}

// TransportListens 本进程是否需要绑定监听端口：
// 源默认监听（除非 SourceDials）、汇默认不监听（除非 SinkListens）、
// relay 下游侧恒监听
func TransportListens() bool {
	switch *Mode {
	case "reality":
		return !SourceDials
	case "mirror":
		return SinkListens
	case "relay":
		return true
	}
	return false
}

// TransportDials 本进程是否主动拨出：与 TransportListens 对偶，
// relay 上游侧恒拨出
func TransportDials() bool {
	switch *Mode {
	case "reality":
		return SourceDials
	case "mirror":
		return !SinkListens
	case "relay":
		return true
	}
	return false
}

// PrintUsage 输出用法说明。
// 用户主动 --help 时应写入 stdout；参数解析出错被动打印时写入 stderr
func PrintUsage(w io.Writer) {
	fmt.Fprintf(w, "Local Mirror - one-way directory mirroring over TCP\n\n")
	fmt.Fprintf(w, "Mirrors a source directory to a sink in real time. Data direction\n")
	fmt.Fprintf(w, "(--send/--receive) and transport direction (--connect/--listen) are\n")
	fmt.Fprintf(w, "independent: either end can dial or listen. The sync root is set with -p\n")
	fmt.Fprintf(w, "and defaults to the current working directory.\n\n")

	fmt.Fprintf(w, "Usage:\n")
	fmt.Fprintf(w, "  local-mirror [flags]\n")
	fmt.Fprintf(w, "  local-mirror ./dir @host[:port]      push ./dir to the listening sink\n")
	fmt.Fprintf(w, "  local-mirror @host[:port] ./dir      pull into ./dir from the listening source\n\n")

	fmt.Fprintf(w, "Direction (what this end is):\n")
	fmt.Fprintf(w, "      --send                   this directory is the source: data flows out\n")
	fmt.Fprintf(w, "      --receive                this directory is the sink: data flows in;\n")
	fmt.Fprintf(w, "                               additive by default, --allow-delete for a faithful mirror.\n")
	fmt.Fprintf(w, "                               Give both to relay (receive upstream, serve downstream)\n\n")

	fmt.Fprintf(w, "Transport (who dials whom; independent of direction):\n")
	fmt.Fprintf(w, "      --connect host[:port]    dial the peer. Port omitted: a dialing sink scans\n")
	fmt.Fprintf(w, "                               %d-%d, a dialing source uses %d. Domain names are\n", DefaultPort, DefaultPort+PortScanRange-1, DefaultPort)
	fmt.Fprintf(w, "                               re-resolved on every reconnect (DDNS-friendly)\n")
	fmt.Fprintf(w, "      --listen                 wait for the peer to dial in; binds the first free\n")
	fmt.Fprintf(w, "                               port from %d (IPv4+IPv6, printed at startup)\n", DefaultPort)
	fmt.Fprintf(w, "                               Defaults: --send listens, --receive connects\n\n")

	fmt.Fprintf(w, "LAN discovery:\n")
	fmt.Fprintf(w, "  A --receive with neither --connect nor --listen scans the local network\n")
	fmt.Fprintf(w, "  for sources over UDP and, if several answer, lets you pick one. It is the\n")
	fmt.Fprintf(w, "  zero-config path for two machines on the same LAN. Discovery does not cross\n")
	fmt.Fprintf(w, "  VPNs, subnets or firewalls: reach those with --connect <host> instead.\n\n")

	fmt.Fprintf(w, "Flags:\n")
	fmt.Fprintf(w, "  -p, --path string            sync root, defaults to the working directory\n")
	fmt.Fprintf(w, "  -l, --loglevel string        log level: debug, info, warn, error (default \"error\")\n")
	fmt.Fprintf(w, "  -c, --cooldown int           full-rescan safety-net interval in seconds, sink side;\n")
	fmt.Fprintf(w, "                               changes are pushed in real time, this is the backstop (default 1800)\n")
	fmt.Fprintf(w, "  -f, --filebuffersize uint    transfer chunk size in bytes, source side (default 65536)\n")
	fmt.Fprintf(w, "  -a, --alias string           instance name shown in discovery lists; defaults to hostname\n")
	fmt.Fprintf(w, "  -i, --ignore string          extra ignore patterns (comma-separated), matched per path\n")
	fmt.Fprintf(w, "                               segment, * ? [] globs supported. Server: never scanned or\n")
	fmt.Fprintf(w, "                               served; client: never downloaded or deleted.\n")
	fmt.Fprintf(w, "                               Defaults: .local-mirror (forced), plus .git and .DS_Store\n")
	fmt.Fprintf(w, "                               (removable — prefix with ! to sync them, e.g. -i '!.git').\n")
	fmt.Fprintf(w, "                               Also read from .local-mirror/ignore (one per line, # comments;\n")
	fmt.Fprintf(w, "                               restart to apply)\n")
	fmt.Fprintf(w, "      --allow-delete           delete local files that no longer exist upstream\n")
	fmt.Fprintf(w, "                               (off by default: additive sync only)\n")
	fmt.Fprintf(w, "      --allow-critical         allow syncing on critical paths (~, /etc, system trees),\n")
	fmt.Fprintf(w, "                               which are refused outright by default. The first overwrite\n")
	fmt.Fprintf(w, "                               backs the original up to .local-mirror/backups; deletion\n")
	fmt.Fprintf(w, "                               still requires --allow-delete on top\n")
	fmt.Fprintf(w, "  -k, --secret string          transport encryption key (Noise NNpsk0), must match on both\n")
	fmt.Fprintf(w, "                               ends. Env: LOCAL_MIRROR_SECRET. Resolution order:\n")
	fmt.Fprintf(w, "                               explicit -k > .local-mirror/key file > plaintext\n")
	fmt.Fprintf(w, "      --gen-key                generate a strong random key into .local-mirror/key (600),\n")
	fmt.Fprintf(w, "                               print it to the terminal, then exit; add run flags (e.g. -m)\n")
	fmt.Fprintf(w, "                               to generate and start in one go. Refuses to overwrite an\n")
	fmt.Fprintf(w, "                               existing key (--force to regenerate)\n")
	fmt.Fprintf(w, "      --status                 live status dashboard for the running instance (refreshes\n")
	fmt.Fprintf(w, "                               in a terminal, prints once when piped). Reads the sync\n")
	fmt.Fprintf(w, "                               root from -p or the current directory; a separate,\n")
	fmt.Fprintf(w, "                               read-only command that never disturbs the daemon\n")
	fmt.Fprintf(w, "      --all                    with --status or --heat: show every local-mirror running\n")
	fmt.Fprintf(w, "                               on this host (discovered from the process table)\n")
	fmt.Fprintf(w, "      --heat                   directory heat table for a running source: which dirs are\n")
	fmt.Fprintf(w, "                               watched in real time vs lazily polled. Read-only, reads\n")
	fmt.Fprintf(w, "                               .local-mirror/heat.json (like --status; -p or cwd, or --all)\n")
	fmt.Fprintf(w, "      --show-key               print the key file to the terminal and exit\n")
	fmt.Fprintf(w, "      --no-encrypt             force plaintext even when a key file exists\n")
	fmt.Fprintf(w, "      --force                  with --gen-key: overwrite the existing key file\n")
	fmt.Fprintf(w, "      --config string          YAML config running multiple tasks under a supervisor\n")
	fmt.Fprintf(w, "                               (one child process per task, crash backoff restart;\n")
	fmt.Fprintf(w, "                               see deploy/local-mirror.example.yml)\n")
	fmt.Fprintf(w, "  -h, --help                   show this help\n")
	fmt.Fprintf(w, "  -v, --version                show version\n\n")

	fmt.Fprintf(w, "Files (under the sync root):\n")
	fmt.Fprintf(w, "  .local-mirror/key              transport key (600; auto-loaded when -k is omitted,\n")
	fmt.Fprintf(w, "                                 never synced). Do not delete on the listening side:\n")
	fmt.Fprintf(w, "                                 regenerating disconnects every dialer\n")
	fmt.Fprintf(w, "  .local-mirror/status.json      runtime status, written only while --status watches (discardable)\n")
	fmt.Fprintf(w, "  .local-mirror/heat.json        directory heat table, written only while --heat watches (discardable)\n")
	fmt.Fprintf(w, "  .local-mirror/cache.db         directory tree cache (reused across restarts)\n")
	fmt.Fprintf(w, "  .local-mirror/logs/error.log   runtime log (errors also go to the terminal)\n")
	fmt.Fprintf(w, "  .local-mirror/ignore           ignore patterns (one per line, # comments; merged with -i)\n\n")

	fmt.Fprintf(w, "Examples:\n")
	fmt.Fprintf(w, "  # classic: serve the current directory, mirror it from another machine\n")
	fmt.Fprintf(w, "  local-mirror --send\n")
	fmt.Fprintf(w, "  local-mirror --receive --connect 192.168.1.100 -p /srv/replica\n\n")

	fmt.Fprintf(w, "  # push to a public VPS: the sink listens there, the source dials out\n")
	fmt.Fprintf(w, "  # (edit locally, never ssh in; home stays outbound-only, NAT-friendly)\n")
	fmt.Fprintf(w, "  vps$   local-mirror --receive --listen -p /srv/backup --allow-delete\n")
	fmt.Fprintf(w, "  home$  local-mirror --send --connect vps.example.net:%d\n\n", DefaultPort)

	fmt.Fprintf(w, "  # same, rsync-style positional sugar\n")
	fmt.Fprintf(w, "  local-mirror ./proj @vps.example.net:%d\n\n", DefaultPort)

	fmt.Fprintf(w, "  # receive with LAN discovery (interactive pick)\n")
	fmt.Fprintf(w, "  local-mirror --receive -p /srv/replica\n\n")

	fmt.Fprintf(w, "  # relay: receive from upstream while serving downstream (A -> B -> C)\n")
	fmt.Fprintf(w, "  local-mirror --send --receive --connect 192.168.1.100 -p /srv/relay\n\n")

	fmt.Fprintf(w, "  # transport encryption, self-managed key: generate on the listening end,\n")
	fmt.Fprintf(w, "  # dial in with it once (the dialer saves it and -k can then be omitted)\n")
	fmt.Fprintf(w, "  local-mirror --gen-key --send\n")
	fmt.Fprintf(w, "  local-mirror --receive --connect 192.168.1.100 -k <generated-key>\n\n")

	fmt.Fprintf(w, "  # ignore node_modules and all .log files\n")
	fmt.Fprintf(w, "  local-mirror --send -i \"node_modules,*.log\"\n")
}

func init() {
	// flag 包在解析出错时调用 Usage：属于用法错误，输出到 stderr
	flag.Usage = func() {
		PrintUsage(os.Stderr)
	}

	Mode = flag.String("mode", "reality", "run mode: reality (server), mirror (client) or relay")
	flag.StringVar(Mode, "m", "reality", "alias of --mode")

	Path = flag.String("path", "", "sync root, defaults to the working directory")
	flag.StringVar(Path, "p", "", "alias of --path")

	// 默认关闭删除：仅增量同步（create/modify），本地多余文件不删。
	// 这样源端异常清空不会级联删除下游。完全忠实镜像需显式解锁
	AllowDelete = flag.Bool("allow-delete", false, "delete local files that no longer exist upstream (off: additive sync only)")

	AllowCritical = flag.Bool("allow-critical", false, "allow syncing on critical paths (~, /etc, system trees); first overwrite is backed up")

	LogLevel = flag.String("loglevel", "error", "log level: debug, info, warn, error")
	flag.StringVar(LogLevel, "l", "error", "alias of --loglevel")

	CoolDown = flag.Int64("cooldown", 1800, "full-rescan safety-net interval in seconds, client side")
	flag.Int64Var(CoolDown, "c", 1800, "alias of --cooldown")

	FileBufferSize = flag.Uint64("filebuffersize", 64*1024, "transfer chunk size in bytes, server side")
	flag.Uint64Var(FileBufferSize, "f", 64*1024, "alias of --filebuffersize")

	RealityIP = flag.String("realityip", "", "upstream server address (mirror/relay); empty = LAN discovery")
	flag.StringVar(RealityIP, "r", "", "alias of --realityip")

	// 默认值取自环境变量：命令行参数会出现在 ps 输出中，
	// 环境变量方式适合不想暴露口令的场景
	secretDefault := os.Getenv("LOCAL_MIRROR_SECRET")
	Secret = flag.String("secret", secretDefault, "transport encryption passphrase, must match on both ends; empty = plaintext")
	flag.StringVar(Secret, "k", secretDefault, "alias of --secret")

	// 密钥自管理（公网化支柱 C）：监听端生成强随机 key，消灭弱口令
	GenKey = flag.Bool("gen-key", false, "generate a strong random key into .local-mirror/key, print it to the terminal, then exit")
	ShowKey = flag.Bool("show-key", false, "print the existing key file to the terminal and exit")
	NoEncrypt = flag.Bool("no-encrypt", false, "force plaintext even when a key file exists")
	Force = flag.Bool("force", false, "with --gen-key: overwrite an existing key file")

	// 运维观测：读取常驻进程写下的 .local-mirror/status.json 并渲染后退出
	Status = flag.Bool("status", false, "print the running instance's status (from .local-mirror/status.json) and exit")
	All = flag.Bool("all", false, "with --status or --heat: discover and show every local-mirror running on this host")

	// 目录热度观测：读取源侧常驻进程写下的 .local-mirror/heat.json 并渲染后退出。
	// 取代旧的 SIGUSR1+heat.txt——只读子命令、跨平台、多进程各读各目录
	Heat = flag.Bool("heat", false, "print a running source's directory heat table (from .local-mirror/heat.json) and exit")

	Help = flag.Bool("help", false, "show help")
	flag.BoolVar(Help, "h", false, "alias of --help")

	Alias = flag.String("alias", "", "instance name shown in discovery lists; defaults to hostname")
	flag.StringVar(Alias, "a", "", "alias of --alias")

	Ignore = flag.String("ignore", "", "extra ignore patterns, comma-separated")
	flag.StringVar(Ignore, "i", "", "alias of --ignore")

	ConfigFile = flag.String("config", "", "YAML config running multiple tasks under a supervisor; excludes other flags")

	// 方向优先 CLI（公网化支柱 A）：两个正交轴取代不透明的 mode 词汇，
	// -m 降级为废弃别名。方向 --send/--receive × 传输 --connect/--listen
	SendFlag = flag.Bool("send", false, "this directory is the source: data flows out")
	ReceiveFlag = flag.Bool("receive", false, "this directory is the sink: data flows in")
	ConnectTo = flag.String("connect", "", "dial the peer at host[:port]; the peer must be listening")
	ListenFlag = flag.Bool("listen", false, "wait for the peer to dial in")

	Version = flag.Bool("version", false, "show version")
	flag.BoolVar(Version, "v", false, "alias of --version")
}
