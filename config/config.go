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

	// DefaultPort 服务端监听/客户端连接的 TCP 端口
	DefaultPort = 52345
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
	DiffInterval    *int64
	FileBufferSize  *uint64
	RealityIP       *string
	Help            *bool
	Version         *bool
	StartPath       string = ""         // Start path
	InstanceID      uint32 = 0x00000000 // Instance ID
	ProtocolVersion uint16 = 0x0001     // Protocol version
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
	fmt.Fprintf(w, "  reality     服务器模式：监听文件变化，在 TCP %d 端口提供同步服务\n", DefaultPort)
	fmt.Fprintf(w, "  mirror      客户端模式：连接服务器，将其目录镜像到本地\n")
	fmt.Fprintf(w, "              注意：镜像是单向的，客户端本地多余的文件会被删除\n\n")

	fmt.Fprintf(w, "Flags:\n")
	fmt.Fprintf(w, "  -m, --mode string            运行模式: reality(服务器) 或 mirror(客户端) (default \"reality\")\n")
	fmt.Fprintf(w, "  -l, --loglevel string        日志级别: debug, info, warn, error (default \"error\")\n")
	fmt.Fprintf(w, "  -c, --cooldown int           全量扫描间隔(秒)，仅客户端: 递归比对整棵目录树的周期 (default 300)\n")
	fmt.Fprintf(w, "  -d, --diffinterval int       变更追踪间隔(秒)，仅客户端: 向服务器查询增量变更的周期 (default 10)\n")
	fmt.Fprintf(w, "  -f, --filebuffersize uint    文件传输分块大小(字节)，仅服务端 (default 65536)\n")
	fmt.Fprintf(w, "  -r, --realityip string       服务器IP地址，仅客户端；空值回退为本机 127.0.0.1\n")
	fmt.Fprintf(w, "  -h, --help                   显示帮助信息\n")
	fmt.Fprintf(w, "  -v, --version                显示版本信息\n\n")

	fmt.Fprintf(w, "Files:\n")
	fmt.Fprintf(w, "  ./.local-mirror/cache.db         目录树缓存（每次启动重建）\n")
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

	fmt.Fprintf(w, "  # 客户端：每 5 秒查询增量变更，每 60 秒做一次全量扫描\n")
	fmt.Fprintf(w, "  local-mirror -m mirror -r 192.168.1.100 -d 5 -c 60\n\n")

	fmt.Fprintf(w, "  # 服务端：调大传输分块到 128KB\n")
	fmt.Fprintf(w, "  local-mirror -m reality -f 131072\n")
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

	CoolDown = flag.Int64("cooldown", 300, "全量扫描间隔(秒)，仅客户端: 递归比对整棵目录树的周期")
	flag.Int64Var(CoolDown, "c", 300, "同 --cooldown")

	DiffInterval = flag.Int64("diffinterval", 10, "变更追踪间隔(秒)，仅客户端: 向服务器查询增量变更的周期")
	flag.Int64Var(DiffInterval, "d", 10, "同 --diffinterval")

	FileBufferSize = flag.Uint64("filebuffersize", 64*1024, "文件传输分块大小(字节)，仅服务端，默认 64KB")
	flag.Uint64Var(FileBufferSize, "f", 64*1024, "同 --filebuffersize")

	RealityIP = flag.String("realityip", "", "服务器IP地址，默认为空表示连接本机 (仅客户端)")
	flag.StringVar(RealityIP, "r", "", "同 --realityip")

	Help = flag.Bool("help", false, "显示帮助信息")
	flag.BoolVar(Help, "h", false, "同 --help")

	Version = flag.Bool("version", false, "显示版本信息")
	flag.BoolVar(Version, "v", false, "同 --version")
}
