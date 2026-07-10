package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// resetIgnore 恢复内置默认与旗子，避免测试间串扰
func resetIgnore(t *testing.T) {
	t.Helper()
	old := IgnoreFileList
	oldFlag := *Ignore
	IgnoreFileList = []string{".local-mirror", ".git", ".DS_Store"}
	*Ignore = ""
	t.Cleanup(func() {
		IgnoreFileList = old
		*Ignore = oldFlag
	})
}

func TestLoadIgnoreListDefaultsOnly(t *testing.T) {
	resetIgnore(t)
	dir := t.TempDir() // 无 .local-mirror/ignore 文件，静默跳过
	if err := LoadIgnoreList(dir); err != nil {
		t.Fatalf("LoadIgnoreList: %v", err)
	}
	want := []string{".local-mirror", ".git", ".DS_Store"}
	if !slices.Equal(IgnoreFileList, want) {
		t.Errorf("got %v, want %v", IgnoreFileList, want)
	}
}

func TestLoadIgnoreListMergeFlagAndFile(t *testing.T) {
	resetIgnore(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".local-mirror"), 0755); err != nil {
		t.Fatal(err)
	}
	content := "# 构建产物\ndist\n\n  node_modules  \n*.log\n.git\n" // 注释/空行/首尾空白/与默认重复
	if err := os.WriteFile(filepath.Join(dir, ".local-mirror", "ignore"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	*Ignore = "node_modules, coverage,,*.tmp , " // 与文件重复项、尾随逗号、空白

	if err := LoadIgnoreList(dir); err != nil {
		t.Fatalf("LoadIgnoreList: %v", err)
	}
	want := []string{".local-mirror", ".git", ".DS_Store", "node_modules", "coverage", "*.tmp", "dist", "*.log"}
	if !slices.Equal(IgnoreFileList, want) {
		t.Errorf("got  %v\nwant %v", IgnoreFileList, want)
	}
}

func TestLoadIgnoreListBadPattern(t *testing.T) {
	resetIgnore(t)
	*Ignore = "[" // 未闭合字符类，filepath.Match 报 ErrBadPattern
	err := LoadIgnoreList(t.TempDir())
	if err == nil {
		t.Fatal("bad pattern accepted")
	}
}

func TestLoadIgnoreListMandatoryEntry(t *testing.T) {
	resetIgnore(t)
	if err := LoadIgnoreList(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(IgnoreFileList, ".local-mirror") {
		t.Error(".local-mirror 强制项丢失")
	}
}
