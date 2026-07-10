package logger

import (
	"fmt"
	"io"
	"local-mirror/config"
	"os"
	"path/filepath"

	log "github.com/sirupsen/logrus"
)

type SimpleFormatter struct{}

func (f *SimpleFormatter) Format(entry *log.Entry) ([]byte, error) {
	timestamp := entry.Time.Format("2006-01-02 15:04:05.000")
	logLine := fmt.Sprintf("%s [%s] %s\n", timestamp, entry.Level.String(), entry.Message)
	return []byte(logLine), nil
}

// getLogDir 日志目录位于同步根目录下，而非进程 CWD——
// 支持 -p 指定目录后从任意位置（如 systemd）启动
func getLogDir() string {
	return filepath.Join(config.StartPath, ".local-mirror", "logs")
}

// LogPath 返回日志文件路径，供启动信息展示
func LogPath() string {
	return filepath.Join(getLogDir(), "error.log")
}

func InitLogger() {
	// 日志同时写入文件和 stderr：
	// 错误必须让终端上的用户看得见，只写文件会让进程"无声退出"。
	// 文件侧走基于大小的轮转 writer，长驻进程不会写满磁盘
	output := io.Writer(os.Stderr)
	if err := os.MkdirAll(getLogDir(), 0755); err != nil {
		log.Warnf("创建日志目录失败，日志仅输出到终端: %v", err)
	} else {
		rw, err := newRotatingWriter(LogPath(), logMaxSize, logMaxFiles)
		if err != nil {
			log.Warnf("日志文件打开失败，日志仅输出到终端: %v", err)
		} else {
			output = io.MultiWriter(rw, os.Stderr)
		}
	}
	log.SetOutput(output)
	log.SetFormatter(&SimpleFormatter{})
	switch *config.LogLevel {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	default:
		log.SetLevel(log.ErrorLevel)
	}
}
