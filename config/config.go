package config

import (
	"flag"
	"fmt"
	"os"
)

const (
	// Mode constants
	RealityMode = 0x0001
	MirrorMode  = 0x0002
)

var (
	IgnoreFileList = []string{"Library", ".gitingore", ".git", "node_modules", ".github", ".local-mirror", ".DS_Store", "server.log", "largeFile.log", ".local-mirror.db"}
)

var (
	ModeMap = map[string]uint8{
		"reality": RealityMode,
		"mirror":  MirrorMode,
	}
)

var (
	Mode             *string
	LogLevel         *string
	CoolDown         *int64
	FileBufferSize   *uint64
	MemFileThreshold *uint64
	RealityIP        *string
	Help             *bool
	Version          *bool
	StartPath        string = ""         // Start path
	InstanceID       uint32 = 0x00000000 // Instance ID
	ProtocolVersion  uint16 = 0x0001     // Protocol version
	StartTime        int64  = 0          // Start time
)

func init() {
	// 自定义用法信息
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Local Mirror - 本地目录镜像同步工具\n\n")
		fmt.Fprintf(os.Stderr, "一个用于本地目录与远程服务器之间进行文件同步的工具。\n")
		fmt.Fprintf(os.Stderr, "支持服务器模式(reality)和客户端模式(mirror)。\n\n")

		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  local-mirror [flags]\n\n")

		fmt.Fprintf(os.Stderr, "Available Commands:\n")
		fmt.Fprintf(os.Stderr, "  reality     启动服务器模式，监听文件变化并提供同步服务\n")
		fmt.Fprintf(os.Stderr, "  mirror      启动客户端模式，连接到服务器并同步文件\n\n")

		fmt.Fprintf(os.Stderr, "Flags:\n")
		fmt.Fprintf(os.Stderr, "  -m, --mode string            运行模式: reality(服务器) 或 mirror(客户端) (default \"reality\")\n")
		fmt.Fprintf(os.Stderr, "  -l, --loglevel string        日志级别: debug, info, warn, error (default \"error\")\n")
		fmt.Fprintf(os.Stderr, "  -cd, --cooldown int          冷却时间(秒): 服务器目录扫描间隔，客户端下载后等待时间 (default 300)\n")
		fmt.Fprintf(os.Stderr, "  -f, --filebuffersize uint    文件缓冲区大小，单位字节 (default 65536)\n")
		fmt.Fprintf(os.Stderr, "  -t, --memfilethreshold uint  内存文件阈值，超过此大小使用磁盘存储 (default 655360)\n")
		fmt.Fprintf(os.Stderr, "  -r, --realityip string       服务器IP地址，空值表示自动检测 (仅客户端模式)\n")
		fmt.Fprintf(os.Stderr, "  -h, --help                   显示帮助信息\n")
		fmt.Fprintf(os.Stderr, "  -v, --version                显示版本信息\n\n")

		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  # 启动服务器模式\n")
		fmt.Fprintf(os.Stderr, "  local-mirror --mode reality\n")
		fmt.Fprintf(os.Stderr, "  local-mirror -m reality\n\n")

		fmt.Fprintf(os.Stderr, "  # 启动客户端模式并连接到指定服务器\n")
		fmt.Fprintf(os.Stderr, "  local-mirror --mode mirror --realityip 192.168.1.100\n")
		fmt.Fprintf(os.Stderr, "  local-mirror -m mirror -r 192.168.1.100\n\n")

		fmt.Fprintf(os.Stderr, "  # 开启调试模式\n")
		fmt.Fprintf(os.Stderr, "  local-mirror --mode reality --loglevel debug\n")
		fmt.Fprintf(os.Stderr, "  local-mirror -m reality -l debug\n\n")

		fmt.Fprintf(os.Stderr, "  # 自定义配置参数\n")
		fmt.Fprintf(os.Stderr, "  local-mirror -m reality -c 600 -f 131072 -t 1048576\n\n")

		fmt.Fprintf(os.Stderr, "Use \"local-mirror --help\" for more information about this command.\n")
	}

	Mode = flag.String("mode", "reality", "运行模式: reality(服务器) 或 mirror(客户端)")
	flag.StringVar(Mode, "m", "reality", "同 --mode")

	LogLevel = flag.String("loglevel", "error", "日志级别: debug, info, warn, error")
	flag.StringVar(LogLevel, "l", "error", "同 --loglevel")

	CoolDown = flag.Int64("cooldown", 300, "冷却时间(秒): 服务器全局目录树遍历间隔，客户端下载完成后等待时间")
	flag.Int64Var(CoolDown, "cd", 300, "同 --cooldown")

	FileBufferSize = flag.Uint64("filebuffersize", 64*1024, "文件缓冲区大小，默认 64KB")
	flag.Uint64Var(FileBufferSize, "f", 64*1024, "同 --filebuffersize")

	MemFileThreshold = flag.Uint64("memfilethreshold", 64*1024*10, "内存文件阈值，超过此大小的文件将使用磁盘存储")
	flag.Uint64Var(MemFileThreshold, "t", 64*1024*10, "同 --memfilethreshold")

	RealityIP = flag.String("realityip", "", "服务器IP地址，默认为空表示自动检测 (仅客户端)")
	flag.StringVar(RealityIP, "r", "", "同 --realityip")

	Help = flag.Bool("help", false, "显示帮助信息")
	flag.BoolVar(Help, "h", false, "同 --help")

	Version = flag.Bool("version", false, "显示版本信息")
	flag.BoolVar(Version, "v", false, "同 --version")
}
