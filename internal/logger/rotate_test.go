package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "error.log")
	// 单文件上限 100 字节，保留 2 个历史
	w, err := newRotatingWriter(path, 100, 2)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	// Windows 上打开中的文件无法删除，必须先关闭 TempDir 清理才能成功
	t.Cleanup(func() { w.Close() })

	line := []byte(strings.Repeat("x", 40) + "\n") // 41 字节/行
	// 写 10 行 = 410 字节，必然触发多次轮转
	for range 10 {
		if _, err := w.Write(line); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	// 当前文件存在且不超上限太多（单行不切分，允许略超）
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat current: %v", err)
	}
	if info.Size() > 100 {
		t.Errorf("当前文件 %d 字节，超过上限 100", info.Size())
	}

	// 历史文件不超过 maxFiles 个：.1/.2 可能存在，.3 绝不该存在
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("超过保留数的历史文件 .3 不该存在")
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Error("轮转后应存在 .1 历史文件")
	}
}

func TestRotatingWriterDefaults(t *testing.T) {
	dir := t.TempDir()
	w, err := newRotatingWriter(filepath.Join(dir, "e.log"), 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	if w.maxSize != 10*1024*1024 || w.maxFiles != 3 {
		t.Errorf("默认值未生效: size=%d files=%d", w.maxSize, w.maxFiles)
	}
}

// TestRotatingWriterReopenSize 重开时应从已有文件大小接续，不从 0 计
func TestRotatingWriterReopenSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "error.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("y", 50)), 0644); err != nil {
		t.Fatal(err)
	}
	w, err := newRotatingWriter(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	if w.size != 50 {
		t.Errorf("重开后 size=%d，应为已有文件大小 50", w.size)
	}
}
