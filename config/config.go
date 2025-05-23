package config

import (
	"flag"
)

var (
	Mode                    = flag.String("mode", "reality", "运行模式: reality 或 mirror")
	LogLevel                = flag.String("logLevel", "error", "日志级别: debug, info, warn, error")
	StartPath        string = ""              // 启动路径
	InstanceID       uint32 = 0x00000000      // 实例ID
	Version          uint16 = 0x0001          // 协议版本
	StartTime        int64  = 0               // 启动时间
	FileBufferSize   uint64 = 64 * 1024       // 文件缓冲区大小，默认64KB
	MemFileThreshold uint64 = 5 * 1024 * 1024 // 内存文件大小阈值，默认5MB
)
