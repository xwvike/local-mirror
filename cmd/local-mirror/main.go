package main

import (
	"flag"
	"fmt"
	"local-mirror/config"
	app "local-mirror/internal"
	"local-mirror/internal/logger"
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
	wd, err := os.Getwd()
	if err != nil {
		// 此时日志尚未初始化，直接写 stderr
		fmt.Fprintf(os.Stderr, "local-mirror: 获取当前工作目录失败: %v\n", err)
		os.Exit(1)
	}
	config.StartPath = wd
}

func main() {
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

	// 用法错误：信息到 stderr，退出码 2（与 flag 包解析失败时的约定一致）
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "local-mirror: 未知参数: %v\n请使用 --help 查看用法\n", flag.Args())
		os.Exit(2)
	}
	if _, ok := config.ModeMap[*config.Mode]; !ok {
		fmt.Fprintf(os.Stderr, "local-mirror: 无效的运行模式 %q (可选: reality, mirror)\n", *config.Mode)
		os.Exit(2)
	}
	switch *config.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		fmt.Fprintf(os.Stderr, "local-mirror: 无效的日志级别 %q (可选: debug, info, warn, error)\n", *config.LogLevel)
		os.Exit(2)
	}

	logger.InitLogger()
	printBanner()
	log.Infof("启动: version=%s mode=%s instance=%08x root=%s", version, *config.Mode, config.InstanceID, config.StartPath)

	tree.InitDB()
	defer func() {
		if tree.DB != nil {
			if err := tree.DB.Close(); err != nil {
				log.Errorf("关闭数据库时出错: %v", err)
			}
		}
	}()
	app.App()
}

// printBanner 向 stdout 输出启动状态。
// 长驻进程默认日志级别下终端不应完全静默，用户需要知道进程在做什么
func printBanner() {
	fmt.Printf("Local Mirror %s (协议版本 %d)\n", version, config.ProtocolVersion)
	fmt.Printf("  模式:     %s\n", *config.Mode)
	fmt.Printf("  同步目录: %s\n", config.StartPath)
	fmt.Printf("  实例ID:   %08x\n", config.InstanceID)
	fmt.Printf("  进程PID:  %d\n", os.Getpid())
	fmt.Printf("  日志:     %s (级别: %s)\n", logger.LogPath(), *config.LogLevel)
	if *config.Mode == "reality" {
		fmt.Printf("  监听地址: 0.0.0.0:%d\n", config.DefaultPort)
	} else {
		ip := *config.RealityIP
		if ip == "" {
			ip = "127.0.0.1"
		}
		fmt.Printf("  服务器:   %s:%d\n", ip, config.DefaultPort)
	}
}
