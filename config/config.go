package config

import (
	"flag"
)

const (
	// Mode constants
	RealityMode = 0x0001
	MirrorMode  = 0x0002
)

var (
	ModeMap = map[string]uint8{
		"reality": RealityMode,
		"mirror":  MirrorMode,
	}
)

var (
	Mode                    = flag.String("mode", "reality", "运行模式: reality 或 mirror")
	LogLevel                = flag.String("logLevel", "error", "日志级别: debug, info, warn, error")
	CoolDown                = flag.Int64("coolDown", 300, "冷静时间（秒）：服务端用于全局遍历目录树的间隔时间，客户端用于下载完成后的等待时间")
	FileBufferSize          = flag.Uint64("fileBufferSize", 64*1024, "文件缓冲区大小，默认64KB")
	MemFileThreshold        = flag.Uint64("memFileThreshold", 64*1024*10, "内存文件阈值，超过此大小则使用磁盘文件")
	StartPath        string = ""         // 启动路径
	InstanceID       uint32 = 0x00000000 // 实例ID
	Version          uint16 = 0x0001     // 协议版本
	StartTime        int64  = 0          // 启动时间
)
