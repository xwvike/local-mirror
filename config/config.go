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
	IgnoreFileList = []string{"Library", ".gitingore", ".git", "node_modules", ".github", ".local-mirror", ".DS_Store", "server.log", "largeFile.log", ".local-mirror.db"}
)

var (
	ModeMap = map[string]uint8{
		"reality": RealityMode,
		"mirror":  MirrorMode,
	}
)

var (
	Mode                    = flag.String("mode", "reality", "Running mode: reality or mirror")
	LogLevel                = flag.String("loglevel", "error", "Log level: debug, info, warn, error")
	CoolDown                = flag.Int64("cooldown", 300, "Cool down time (seconds): Interval for global directory tree traversal on server, waiting time after download completion on client")
	FileBufferSize          = flag.Uint64("filebuffersize", 64*1024, "File buffer size, default 64KB")
	MemFileThreshold        = flag.Uint64("memfilethreshold", 64*1024*10, "Memory file threshold, files larger than this will use disk storage")
	RealityIP               = flag.String("realityip", "", "Server IP address, default empty means auto-detect - (client only)")
	StartPath        string = ""         // Start path
	InstanceID       uint32 = 0x00000000 // Instance ID
	Version          uint16 = 0x0001     // Protocol version
	StartTime        int64  = 0          // Start time
)
