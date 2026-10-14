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
	closed   bool // Close 被显式调用过；与"重开失败导致 file==nil"区分开
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

// open 打开（追加）当前日志文件。失败时把 w.file 置空：rotate 先关旧句柄再
// 调用本函数，若这里失败而 w.file 仍指向那个已关闭的句柄，后续 Write 会一直
// 往关闭的 fd 上写（返回 ErrClosed 却被当成普通写错误），日志静默丢到进程结束
func (w *rotatingWriter) open() error {
	w.file = nil
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

	if w.closed {
		return 0, fmt.Errorf("log file not open")
	}
	// 上一次轮转/重开失败过：先自愈。不自愈的话一次瞬时失败（磁盘满、
	// 日志目录被挪走）会让本进程余生的日志全部丢弃，而排障恰恰靠日志
	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, fmt.Errorf("reopening log file: %w", err)
		}
	}
	// 单条超过上限也整条写入（不切分日志行），写完再滚动
	if w.size+int64(len(p)) > w.maxSize && w.size > 0 {
		if err := w.rotate(); err != nil {
			fmt.Fprintf(os.Stderr, "log rotation failed: %v\n", err)
		}
	}
	// 轮转要先关当前文件（Windows 上打开中的文件改不了名），失败时连重开也没成，
	// 此时没有可写句柄：本条日志只能上报错误，下一条进来会再试一次自愈
	if w.file == nil {
		return 0, fmt.Errorf("log file not open after failed rotation")
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
	w.closed = true
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
