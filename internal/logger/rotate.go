package logger

import (
	"fmt"
	"os"
	"sync"
)

// rotatingWriter 基于大小的日志轮转 writer（手写，避免引入 lumberjack 依赖）。
// 单文件超过 maxSize 时滚动：error.log → error.log.1 → … → error.log.N，
// 最旧的被丢弃。长驻进程由此不会写满磁盘。
type rotatingWriter struct {
	mu       sync.Mutex
	path     string // 当前日志文件路径
	maxSize  int64  // 单文件字节上限
	maxFiles int    // 保留的历史文件数（不含当前）
	size     int64  // 当前文件已写字节
	file     *os.File
}

// newRotatingWriter 打开（追加）日志文件并初始化轮转状态。
// maxSize<=0 或 maxFiles<0 时用安全默认（10MB / 保留 3 个）
func newRotatingWriter(path string, maxSize int64, maxFiles int) (*rotatingWriter, error) {
	if maxSize <= 0 {
		maxSize = 10 * 1024 * 1024
	}
	if maxFiles < 0 {
		maxFiles = 3
	}
	w := &rotatingWriter{path: path, maxSize: maxSize, maxFiles: maxFiles}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.file = f
	w.size = info.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return 0, fmt.Errorf("log file not open")
	}
	// 单条超过上限也整条写入（不切分日志行），写完再滚动
	if w.size+int64(len(p)) > w.maxSize && w.size > 0 {
		if err := w.rotate(); err != nil {
			// 轮转失败不丢日志：继续写当前文件（宁可超限也不静默丢失）
			fmt.Fprintf(os.Stderr, "log rotation failed, continuing with current file: %v\n", err)
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Close 关闭当前日志文件。Windows 上打开中的文件无法被删除，
// 持有者必须显式关闭句柄，目录清理（如测试的 TempDir）才能成功。
// 关闭后再 Write 返回"日志文件未打开"错误，不会 panic。
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// rotate 关闭当前文件，把历史文件依次后移一位后重开。
// .N 被丢弃，.（N-1）→.N，…，.1→.2，当前→.1
func (w *rotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	oldest := fmt.Sprintf("%s.%d", w.path, w.maxFiles)
	_ = os.Remove(oldest) // 最旧的丢弃；不存在也无妨
	for i := w.maxFiles - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.path, i)
		dst := fmt.Sprintf("%s.%d", w.path, i+1)
		_ = os.Rename(src, dst) // 中间某个不存在时忽略
	}
	if w.maxFiles >= 1 {
		if err := os.Rename(w.path, w.path+".1"); err != nil {
			// 改名失败则重开原文件，避免丢失写入能力
			_ = w.open()
			return err
		}
	}
	return w.open()
}

// 轮转参数（暂用固定默认，未来可经 flag/config 暴露）
var (
	logMaxSize  int64 = 10 * 1024 * 1024 // 单文件 10MB
	logMaxFiles       = 3                // 保留最近 3 个历史文件
)
