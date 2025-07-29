package logger

import (
	"fmt"
	"local-mirror/config"
	"os"
	"path/filepath"
	"runtime"

	log "github.com/sirupsen/logrus"
)

type SimpleFormatter struct{}

func (f *SimpleFormatter) Format(entry *log.Entry) ([]byte, error) {
	timestamp := entry.Time.Format("2006-01-02 15:04:05.000")
	logLine := fmt.Sprintf("%s [%s] %s\n", timestamp, entry.Level.String(), entry.Message)
	return []byte(logLine), nil
}

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

func Initialize() {
	levelMap := map[string]log.Level{
		"debug": log.DebugLevel,
		"info":  log.InfoLevel,
		"warn":  log.WarnLevel,
		"error": log.ErrorLevel,
	}

	level, exists := levelMap[*config.LogLevel]
	if !exists {
		level = log.ErrorLevel
	}

	log.SetLevel(level)
	log.SetFormatter(&SimpleFormatter{})

	logDir := getLogDir()
	os.MkdirAll(logDir, 0755)

	logFile := filepath.Join(logDir, "app.log")
	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		log.SetOutput(file)
	} else {
		log.Warn("Failed to log to file, using default stderr")
	}
}
