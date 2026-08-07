//go:build unix

package safety

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVerifyNoSymlinkComponents 验证 SEC-03/04 的逐级符号链接防护：中间或末段任一路径组件
// 是符号链接即拒绝（挡住「符号链接父目录逃逸」），全真实组件、不存在的叶子、根本身放行。
func TestVerifyNoSymlinkComponents(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "b", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 全真实组件 → nil
	if err := VerifyNoSymlinkComponents(root, filepath.Join("a", "b", "f.txt")); err != nil {
		t.Errorf("正常路径不该报错: %v", err)
	}
	// rel="." → nil
	if err := VerifyNoSymlinkComponents(root, "."); err != nil {
		t.Errorf(". 不该报错: %v", err)
	}
	// 不存在的叶子（父真实）→ nil（安全，其下不可能有符号链接父）
	if err := VerifyNoSymlinkComponents(root, filepath.Join("a", "b", "new.txt")); err != nil {
		t.Errorf("不存在的叶子不该报错: %v", err)
	}

	// 中间组件是指向根外的符号链接 → 拒绝
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Skipf("符号链接不可用，跳过: %v", err)
	}
	if err := VerifyNoSymlinkComponents(root, filepath.Join("evil", "secret")); err == nil {
		t.Error("中间符号链接组件应被拒绝（否则可读/写到 /etc 之类根外目标）")
	}
	if _, err := SafeResolve(root, filepath.Join("evil", "secret")); err == nil {
		t.Error("SafeResolve 应拒绝符号链接父目录")
	}

	// 末段是符号链接 → 拒绝
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "a", "link")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyNoSymlinkComponents(root, filepath.Join("a", "link")); err == nil {
		t.Error("末段符号链接应被拒绝")
	}

	// SafeResolve 正常路径 → 返回拼接路径
	full, err := SafeResolve(root, filepath.Join("a", "b", "f.txt"))
	if err != nil || full != filepath.Join(root, "a", "b", "f.txt") {
		t.Errorf("SafeResolve 正常路径应返回拼接路径，got %q err=%v", full, err)
	}
}
