package logger

import (
	"fmt"
	"io"
	"local-mirror/config"
	"os"
	"path/filepath"
	"strings"
	"sync"

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

// fileOut 日志文件的轮转 writer；建不起来时为 nil（只能写终端）
var fileOut io.Writer

func InitLogger() {
	// 日志同时写入文件和 stderr：
	// 错误必须让终端上的用户看得见，只写文件会让进程"无声退出"。
	// 文件侧走基于大小的轮转 writer，长驻进程不会写满磁盘
	output := io.Writer(os.Stderr)
	if err := os.MkdirAll(getLogDir(), 0755); err != nil {
		log.Warnf("failed to create log directory, logging to terminal only: %v", err)
	} else {
		rw, err := newRotatingWriter(LogPath(), logMaxSize, logMaxFiles)
		if err != nil {
			log.Warnf("failed to open log file, logging to terminal only: %v", err)
		} else {
			fileOut = rw
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

// recentLines 前台实时面板显示的最近日志条数
const recentLines = 3

// recentHook 记下最近几条日志，供前台实时面板显示（日志此时不写终端）
type recentHook struct {
	mu    sync.Mutex
	lines []string
}

func (h *recentHook) Levels() []log.Level { return log.AllLevels }

func (h *recentHook) Fire(e *log.Entry) error {
	level := e.Level.String()
	if e.Level == log.WarnLevel {
		level = "warn"
	}
	msg, _, _ := strings.Cut(e.Message, "\n")
	h.mu.Lock()
	h.lines = append(h.lines, fmt.Sprintf("%s %-5s %s", e.Time.Format("15:04:05"), level, msg))
	if len(h.lines) > recentLines {
		h.lines = h.lines[len(h.lines)-recentLines:]
	}
	h.mu.Unlock()
	return nil
}

var (
	recent      = &recentHook{}
	recentAdded bool
)

// DetachTerminal 日志改为只写文件，最近几条由 Recent 提供（前台实时面板期间）。
// 没有日志文件可写时保持原样并返回 false
func DetachTerminal() bool {
	if fileOut == nil {
		return false
	}
	if !recentAdded {
		log.AddHook(recent)
		recentAdded = true
	}
	log.SetOutput(fileOut)
	return true
}

// AttachTerminal 恢复日志同时写文件与终端
func AttachTerminal() {
	if fileOut != nil {
		log.SetOutput(io.MultiWriter(fileOut, os.Stderr))
	}
}

// Recent 返回最近几条日志（时间、级别、消息首行）
func Recent() []string {
	recent.mu.Lock()
	defer recent.mu.Unlock()
	return append([]string(nil), recent.lines...)
}
