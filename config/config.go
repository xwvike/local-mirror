package config

import (
	"flag"
	"fmt"
	"io"
	"os"
)

const (
	// 运行模式
	RealityMode = 0x0001
	MirrorMode  = 0x0002

	// DefaultPort 端口探测的起始 TCP 端口。
	// 服务端从这里开始寻找第一个可用端口监听；
	// 客户端在 [DefaultPort, DefaultPort+PortScanRange) 范围内逐个握手探测服务端
	DefaultPort = 52345
	// PortScanRange 端口探测范围大小
	PortScanRange = 10
)

var (
	// 忽略列表按路径段精确匹配（见 utils.IsIgnored）
	IgnoreFileList = []string{"Library", ".gitignore", ".git", "node_modules", ".github", ".local-mirror", ".DS_Store", "server.log", "largeFile.log", ".local-mirror.db"}
)

var (
	ModeMap = map[string]uint8{
		"reality": RealityMode,
		"mirror":  MirrorMode,
	}
)

var (
	Mode            *string
	LogLevel        *string
	CoolDown        *int64
	FileBufferSize  *uint64
	RealityIP       *string
	Secret          *string
	Help            *bool
	Version         *bool
	ActualPort      int    = 0          // 服务端实际监听的端口（启动时探测确定）
	StartPath       string = ""         // Start path
	InstanceID      uint32 = 0x00000000 // Instance ID
	ProtocolVersion uint16 = 0x0002     // Protocol version（v2：变更查询改为长轮询推送）
	StartTime       int64  = 0          // Start time
)

// PrintUsage 输出用法说明。
// 用户主动 --help 时应写入 stdout；参数解析出错被动打印时写入 stderr
func PrintUsage(w io.Writer) {
	fmt.Fprintf(w, "Local Mirror - 本地目录镜像同步工具\n\n")
	fmt.Fprintf(w, "把服务端（reality）的目录单向镜像到客户端（mirror）。\n")
	fmt.Fprintf(w, "同步根目录为进程启动时的当前工作目录，请先 cd 到目标目录再运行。\n\n")

	fmt.Fprintf(w, "Usage:\n")
	fmt.Fprintf(w, "  local-mirror [flags]\n\n")

	fmt.Fprintf(w, "Modes (-m/--mode 的取值):\n")
	fmt.Fprintf(w, "  reality     服务器模式：监听文件变化并提供同步服务。\n")
	fmt.Fprintf(w, "              从 TCP %d 起自动选择第一个可用端口，实际端口见启动信息\n", DefaultPort)
	fmt.Fprintf(w, "  mirror      客户端模式：连接服务器，将其目录镜像到本地。\n")
	fmt.Fprintf(w, "              在 %d-%d 端口范围内自动探测服务端\n", DefaultPort, DefaultPort+PortScanRange-1)
	fmt.Fprintf(w, "              注意：镜像是单向的，客户端本地多余的文件会被删除\n\n")

	fmt.Fprintf(w, "Flags:\n")
	fmt.Fprintf(w, "  -m, --mode string            运行模式: reality(服务器) 或 mirror(客户端) (default \"reality\")\n")
	fmt.Fprintf(w, "  -l, --loglevel string        日志级别: debug, info, warn, error (default \"error\")\n")
	fmt.Fprintf(w, "  -c, --cooldown int           全量扫描安全网间隔(秒)，仅客户端: 变更实时推送，此为兜底 (default 1800)\n")
	fmt.Fprintf(w, "  -f, --filebuffersize uint    文件传输分块大小(字节)，仅服务端 (default 65536)\n")
	fmt.Fprintf(w, "  -r, --realityip string       服务器IP地址，仅客户端；空值回退为本机 127.0.0.1\n")
	fmt.Fprintf(w, "  -k, --secret string          传输加密口令（Noise NNpsk0），两端必须一致；\n")
	fmt.Fprintf(w, "                               为空则明文传输。也可用环境变量 LOCAL_MIRROR_SECRET\n")
	fmt.Fprintf(w, "  -h, --help                   显示帮助信息\n")
	fmt.Fprintf(w, "  -v, --version                显示版本信息\n\n")

	fmt.Fprintf(w, "Files:\n")
	fmt.Fprintf(w, "  ./.local-mirror/cache.db         目录树缓存（跨重启复用，加速启动）\n")
	fmt.Fprintf(w, "  ./.local-mirror/logs/error.log   运行日志（错误同时输出到终端）\n\n")

	fmt.Fprintf(w, "Examples:\n")
	fmt.Fprintf(w, "  # 启动服务器模式\n")
	fmt.Fprintf(w, "  local-mirror --mode reality\n")
	fmt.Fprintf(w, "  local-mirror -m reality\n\n")

	fmt.Fprintf(w, "  # 启动客户端模式并连接到指定服务器\n")
	fmt.Fprintf(w, "  local-mirror --mode mirror --realityip 192.168.1.100\n")
	fmt.Fprintf(w, "  local-mirror -m mirror -r 192.168.1.100\n\n")

	fmt.Fprintf(w, "  # 开启调试模式\n")
	fmt.Fprintf(w, "  local-mirror --mode reality --loglevel debug\n")
	fmt.Fprintf(w, "  local-mirror -m reality -l debug\n\n")

	fmt.Fprintf(w, "  # 客户端：把全量扫描安全网间隔调到 1 小时（变更本身实时推送）\n")
	fmt.Fprintf(w, "  local-mirror -m mirror -r 192.168.1.100 -c 3600\n\n")

	fmt.Fprintf(w, "  # 服务端：调大传输分块到 128KB\n")
	fmt.Fprintf(w, "  local-mirror -m reality -f 131072\n\n")

	fmt.Fprintf(w, "  # 开启传输加密（两端使用相同口令）\n")
	fmt.Fprintf(w, "  local-mirror -m reality -k mypassword\n")
	fmt.Fprintf(w, "  local-mirror -m mirror -r 192.168.1.100 -k mypassword\n")
}

func init() {
	// flag 包在解析出错时调用 Usage：属于用法错误，输出到 stderr
	flag.Usage = func() {
		PrintUsage(os.Stderr)
	}

	Mode = flag.String("mode", "reality", "运行模式: reality(服务器) 或 mirror(客户端)")
	flag.StringVar(Mode, "m", "reality", "同 --mode")

	LogLevel = flag.String("loglevel", "error", "日志级别: debug, info, warn, error")
	flag.StringVar(LogLevel, "l", "error", "同 --loglevel")

	CoolDown = flag.Int64("cooldown", 1800, "全量扫描安全网间隔(秒)，仅客户端: 变更实时推送，全量扫描仅作兜底")
	flag.Int64Var(CoolDown, "c", 1800, "同 --cooldown")

	FileBufferSize = flag.Uint64("filebuffersize", 64*1024, "文件传输分块大小(字节)，仅服务端，默认 64KB")
	flag.Uint64Var(FileBufferSize, "f", 64*1024, "同 --filebuffersize")

	RealityIP = flag.String("realityip", "", "服务器IP地址，默认为空表示连接本机 (仅客户端)")
	flag.StringVar(RealityIP, "r", "", "同 --realityip")

	// 默认值取自环境变量：命令行参数会出现在 ps 输出中，
	// 环境变量方式适合不想暴露口令的场景
	secretDefault := os.Getenv("LOCAL_MIRROR_SECRET")
	Secret = flag.String("secret", secretDefault, "传输加密口令，两端一致才能通信；为空则明文传输")
	flag.StringVar(Secret, "k", secretDefault, "同 --secret")

	Help = flag.Bool("help", false, "显示帮助信息")
	flag.BoolVar(Help, "h", false, "同 --help")

	Version = flag.Bool("version", false, "显示版本信息")
	flag.BoolVar(Version, "v", false, "同 --version")
}
