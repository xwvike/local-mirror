package logger

import (
	"fmt"
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

func getLogDir() string {
	return "./.local-mirror/logs"
}

func InitLogger() {

	if err := os.MkdirAll(getLogDir(), 0755); err != nil {
		log.Fatalf("创建日志目录失败: %v", err)
		return
	}
	logPath := filepath.Join(getLogDir(), "error.log")
	fmt.Println("日志文件路径:", logPath)

	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		log.SetOutput(file)
	} else {
		log.Info("日志文件打开失败，使用默认stderr")
	}
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
