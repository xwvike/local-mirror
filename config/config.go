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
	// 运行模式
	RealityMode = 0x0001
	MirrorMode  = 0x0002
	RelayMode   = 0x0003

	// DefaultPort 端口探测的起始 TCP 端口。
	// 服务端从这里开始寻找第一个可用端口监听；
	// 客户端在 [DefaultPort, DefaultPort+PortScanRange) 范围内逐个握手探测服务端
	DefaultPort = 52345
	// PortScanRange 端口探测范围大小
	PortScanRange = 10
)

var (
	// IgnoreFileList 生效的忽略列表：内置默认 + -i/--ignore + .local-mirror/ignore
	// 文件合并去重后的结果（见 LoadIgnoreList，启动时调用一次，不热加载）。
	// 匹配按路径段进行，每段支持 * ? [] 通配符（见 utils.IsIgnored）。
	// 服务端命中即不扫描/不监听（不进树），客户端命中即不同步（不下载也不删除）。
	// 初始值即内置默认：.local-mirror 是状态目录，任何情况下都不得同步（强制项）
	IgnoreFileList = []string{".local-mirror", ".git", ".DS_Store"}
)

var (
	ModeMap = map[string]uint8{
		"reality": RealityMode,
		"mirror":  MirrorMode,
		"relay":   RelayMode,
	}
)

var (
	Mode            *string
	LogLevel        *string
	CoolDown        *int64
	FileBufferSize  *uint64
	RealityIP       *string
	Secret          *string
	Path            *string
	Alias           *string
	Ignore          *string
	ConfigFile      *string
	AllowDelete     *bool
	Help            *bool
	Version         *bool
	ActualPort      int    = 0          // 服务端实际监听的端口（启动时探测确定）
	StartPath       string = ""         // 同步根目录（-p 指定，默认为当前工作目录）
	InstanceID      uint32 = 0x00000000 // Instance ID
	ProtocolVersion uint16 = 0x0002     // Protocol version（v2：变更查询改为长轮询推送）
	StartTime       int64  = 0          // Start time

	// AliasName 解析后的最终实例别名（--alias → 主机名 → "local-mirror"），
	// 服务端在 UDP 发现应答中广播，供客户端选择列表展示
	AliasName string = ""
	// DiscoveredAddr/DiscoveredAlias 客户端自动发现选定的上游 "ip:port" 与其别名。
	// 仅在 -r 留空且发现流程成功时非空；InitConn 优先直连该地址
	DiscoveredAddr  string = ""
	DiscoveredAlias string = ""
)

// LoadIgnoreList 合并生效忽略列表：内置默认 + -i/--ignore 逗号分隔条目 +
// <startPath>/.local-mirror/ignore 文件（每行一条，# 注释，空行跳过，
// 文件不存在则静默跳过）。每条模式用 filepath.Match 预校验，非法模式
// （如未闭合的 "["）返回错误。结果去重（保序）后写回 IgnoreFileList。
// 启动时调用一次；运行中修改文件不生效，需重启
func LoadIgnoreList(startPath string) error {
	patterns := append([]string{}, IgnoreFileList...)

	if *Ignore != "" {
		for _, p := range strings.Split(*Ignore, ",") {
			if p = strings.TrimSpace(p); p != "" {
				patterns = append(patterns, p)
			}
		}
	}

	ignoreFile := filepath.Join(startPath, ".local-mirror", "ignore")
	if data, err := os.ReadFile(ignoreFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			patterns = append(patterns, line)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取忽略配置 %s 失败: %w", ignoreFile, err)
	}

	seen := make(map[string]struct{}, len(patterns))
	merged := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if _, err := filepath.Match(p, "x"); err != nil {
			return fmt.Errorf("非法的忽略模式 %q: %w", p, err)
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		merged = append(merged, p)
	}
	IgnoreFileList = merged
	return nil
}

// ServesDownstream 当前模式是否对下游提供同步服务（需要监听端口）
func ServesDownstream() bool {
	return *Mode == "reality" || *Mode == "relay"
}

// SyncsFromUpstream 当前模式是否从上游同步（作为客户端）
func SyncsFromUpstream() bool {
	return *Mode == "mirror" || *Mode == "relay"
}

// PrintUsage 输出用法说明。
// 用户主动 --help 时应写入 stdout；参数解析出错被动打印时写入 stderr
func PrintUsage(w io.Writer) {
	fmt.Fprintf(w, "Local Mirror - 本地目录镜像同步工具\n\n")
	fmt.Fprintf(w, "把服务端（reality）的目录单向、实时地镜像到客户端（mirror），\n")
	fmt.Fprintf(w, "或经中继（relay）逐级传递。同步根目录用 -p 指定，默认为当前工作目录。\n\n")

	fmt.Fprintf(w, "Usage:\n")
	fmt.Fprintf(w, "  local-mirror [flags]\n\n")

	fmt.Fprintf(w, "Modes (-m/--mode 的取值):\n")
	fmt.Fprintf(w, "  reality     服务器模式：监听文件变化并提供同步服务。\n")
	fmt.Fprintf(w, "              从 TCP %d 起自动选择第一个可用端口，实际端口见启动信息\n", DefaultPort)
	fmt.Fprintf(w, "  mirror      客户端模式：连接服务器，将其目录镜像到本地。\n")
	fmt.Fprintf(w, "              -r 留空时自动发现局域网内的服务端（交互终端下列表选择），\n")
	fmt.Fprintf(w, "              连接时在 %d-%d 端口范围内自动探测\n", DefaultPort, DefaultPort+PortScanRange-1)
	fmt.Fprintf(w, "              默认仅增量同步（不删除）；加 --allow-delete 才会删除本地多余文件\n")
	fmt.Fprintf(w, "  relay       中继模式：从上游服务器镜像到本地，同时向下游提供同步服务，\n")
	fmt.Fprintf(w, "              可级联组成 A → B → C 传递链。上游用 -r 指定或自动发现\n\n")

	fmt.Fprintf(w, "Flags:\n")
	fmt.Fprintf(w, "  -m, --mode string            运行模式: reality / mirror / relay (default \"reality\")\n")
	fmt.Fprintf(w, "  -p, --path string            同步根目录，默认为当前工作目录\n")
	fmt.Fprintf(w, "  -l, --loglevel string        日志级别: debug, info, warn, error (default \"error\")\n")
	fmt.Fprintf(w, "  -c, --cooldown int           全量扫描安全网间隔(秒)，仅客户端: 变更实时推送，此为兜底 (default 1800)\n")
	fmt.Fprintf(w, "  -f, --filebuffersize uint    文件传输分块大小(字节)，仅服务端 (default 65536)\n")
	fmt.Fprintf(w, "  -r, --realityip string       上游服务器IP地址（mirror/relay）；留空时自动发现局域网服务端\n")
	fmt.Fprintf(w, "                               （UDP 组播/广播，VPN、跨网段或防火墙环境请用 -r 直连）\n")
	fmt.Fprintf(w, "  -a, --alias string           实例别名，展示在局域网发现列表中；默认为主机名\n")
	fmt.Fprintf(w, "  -i, --ignore string          追加忽略模式（逗号分隔），按路径段匹配，支持 * ? [] 通配符；\n")
	fmt.Fprintf(w, "                               服务端命中即不扫描，客户端命中即不同步（不下载也不删除）。\n")
	fmt.Fprintf(w, "                               内置默认: .local-mirror .git .DS_Store；\n")
	fmt.Fprintf(w, "                               也可写入 .local-mirror/ignore 文件（每行一条，# 注释），改后需重启\n")
	fmt.Fprintf(w, "      --allow-delete           删除同步：允许删除本地多余文件（默认关闭，仅增量同步）\n")
	fmt.Fprintf(w, "                               关键路径（如 ~、/etc、系统目录）上会被拒绝启动\n")
	fmt.Fprintf(w, "  -k, --secret string          传输加密口令（Noise NNpsk0），两端必须一致；\n")
	fmt.Fprintf(w, "                               为空则明文传输。也可用环境变量 LOCAL_MIRROR_SECRET\n")
	fmt.Fprintf(w, "      --config string          多任务 YAML 配置文件，以监督模式同时运行多个任务\n")
	fmt.Fprintf(w, "                               （一任务一子进程，异常退避重启；示例见 deploy/local-mirror.example.yml）\n")
	fmt.Fprintf(w, "  -h, --help                   显示帮助信息\n")
	fmt.Fprintf(w, "  -v, --version                显示版本信息\n\n")

	fmt.Fprintf(w, "Files（位于同步根目录下）:\n")
	fmt.Fprintf(w, "  .local-mirror/cache.db         目录树缓存（跨重启复用，加速启动）\n")
	fmt.Fprintf(w, "  .local-mirror/logs/error.log   运行日志（错误同时输出到终端）\n")
	fmt.Fprintf(w, "  .local-mirror/ignore           忽略模式（每行一条，# 注释；与 -i 合并生效）\n\n")

	fmt.Fprintf(w, "Examples:\n")
	fmt.Fprintf(w, "  # 启动服务器模式（同步当前目录）\n")
	fmt.Fprintf(w, "  local-mirror -m reality\n\n")

	fmt.Fprintf(w, "  # 指定同步目录启动（适合 systemd 等服务化部署，无需 cd）\n")
	fmt.Fprintf(w, "  local-mirror -m reality -p /srv/data\n\n")

	fmt.Fprintf(w, "  # 客户端连接到指定服务器\n")
	fmt.Fprintf(w, "  local-mirror -m mirror -r 192.168.1.100 -p /srv/replica\n\n")

	fmt.Fprintf(w, "  # 客户端自动发现局域网服务端（交互式列表选择）\n")
	fmt.Fprintf(w, "  local-mirror -m mirror -p /srv/replica\n\n")

	fmt.Fprintf(w, "  # 中继：从 192.168.1.100 镜像下来，同时向下游提供服务\n")
	fmt.Fprintf(w, "  local-mirror -m relay -r 192.168.1.100 -p /srv/relay\n\n")

	fmt.Fprintf(w, "  # 开启传输加密（两端使用相同口令）\n")
	fmt.Fprintf(w, "  local-mirror -m reality -k mypassword\n")
	fmt.Fprintf(w, "  local-mirror -m mirror -r 192.168.1.100 -k mypassword\n\n")

	fmt.Fprintf(w, "  # 客户端：把全量扫描安全网间隔调到 1 小时（变更本身实时推送）\n")
	fmt.Fprintf(w, "  local-mirror -m mirror -r 192.168.1.100 -c 3600\n\n")

	fmt.Fprintf(w, "  # 忽略 node_modules 与所有 .log 文件（服务端不扫描 / 客户端不同步）\n")
	fmt.Fprintf(w, "  local-mirror -m reality -i \"node_modules,*.log\"\n")
}

func init() {
	// flag 包在解析出错时调用 Usage：属于用法错误，输出到 stderr
	flag.Usage = func() {
		PrintUsage(os.Stderr)
	}

	Mode = flag.String("mode", "reality", "运行模式: reality(服务器)、mirror(客户端) 或 relay(中继)")
	flag.StringVar(Mode, "m", "reality", "同 --mode")

	Path = flag.String("path", "", "同步根目录，默认为当前工作目录")
	flag.StringVar(Path, "p", "", "同 --path")

	// 默认关闭删除：仅增量同步（create/modify），本地多余文件不删。
	// 这样源端异常清空不会级联删除下游。完全忠实镜像需显式解锁
	AllowDelete = flag.Bool("allow-delete", false, "删除同步：允许删除本地多余文件（默认关闭，仅增量同步）")

	LogLevel = flag.String("loglevel", "error", "日志级别: debug, info, warn, error")
	flag.StringVar(LogLevel, "l", "error", "同 --loglevel")

	CoolDown = flag.Int64("cooldown", 1800, "全量扫描安全网间隔(秒)，仅客户端: 变更实时推送，全量扫描仅作兜底")
	flag.Int64Var(CoolDown, "c", 1800, "同 --cooldown")

	FileBufferSize = flag.Uint64("filebuffersize", 64*1024, "文件传输分块大小(字节)，仅服务端，默认 64KB")
	flag.Uint64Var(FileBufferSize, "f", 64*1024, "同 --filebuffersize")

	RealityIP = flag.String("realityip", "", "上游服务器IP地址（mirror/relay）")
	flag.StringVar(RealityIP, "r", "", "同 --realityip")

	// 默认值取自环境变量：命令行参数会出现在 ps 输出中，
	// 环境变量方式适合不想暴露口令的场景
	secretDefault := os.Getenv("LOCAL_MIRROR_SECRET")
	Secret = flag.String("secret", secretDefault, "传输加密口令，两端一致才能通信；为空则明文传输")
	flag.StringVar(Secret, "k", secretDefault, "同 --secret")

	Help = flag.Bool("help", false, "显示帮助信息")
	flag.BoolVar(Help, "h", false, "同 --help")

	Alias = flag.String("alias", "", "实例别名，服务端在局域网发现中展示；默认为主机名")
	flag.StringVar(Alias, "a", "", "同 --alias")

	Ignore = flag.String("ignore", "", "追加忽略模式（逗号分隔），按路径段匹配，支持 * ? [] 通配符")
	flag.StringVar(Ignore, "i", "", "同 --ignore")

	ConfigFile = flag.String("config", "", "多任务 YAML 配置文件；给出后以监督模式运行，其余参数无效")

	Version = flag.Bool("version", false, "显示版本信息")
	flag.BoolVar(Version, "v", false, "同 --version")
}
