package app

import (
	"fmt"
	"local-mirror/config"
	"os"
	"path/filepath"
	"runtime"

	log "github.com/sirupsen/logrus"
)

func getLogDir() string {
	switch runtime.GOOS {
	case "windows":
		return os.Getenv("USERPROFILE") + "\\AppData\\Local\\local-mirror\\logs"
	case "linux", "freebsd":
		return os.Getenv("HOME") + "/.local-mirror/logs"
	case "darwin":
		return os.Getenv("HOME") + "/Library/Logs/local-mirror"
	}
	executable, err := os.Executable()
	if err == nil {
		return filepath.Join(filepath.Dir(executable), "logs")
	}
	return filepath.Join(".", "logs")
}

func InitLogger() {

	if err := os.MkdirAll(getLogDir(), 0755); err != nil {
		log.Fatalf("创建日志目录失败: %v", err)
		return
	}
	logPath := filepath.Join(getLogDir(), "app.log")
	fmt.Println("日志文件路径:", logPath)

	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		log.SetOutput(file)
	} else {
		log.Info("日志文件打开失败，使用默认stderr")
	}
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})
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
