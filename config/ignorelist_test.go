package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// loadIgnore 以给定 -i 值与可选的 ignore 文件内容跑 LoadIgnoreList，返回生效列表。
// 跑完恢复全局态（*Ignore / IgnoreFileList），避免污染其它测试
func loadIgnore(t *testing.T, iflag, fileContent string) []string {
	t.Helper()
	root := t.TempDir()
	if fileContent != "" {
		dir := filepath.Join(root, ".local-mirror")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ignore"), []byte(fileContent), 0644); err != nil {
			t.Fatal(err)
		}
	}
	oldFlag, oldList := *Ignore, IgnoreFileList
	t.Cleanup(func() { *Ignore = oldFlag; IgnoreFileList = oldList })
	*Ignore = iflag
	if err := LoadIgnoreList(root); err != nil {
		t.Fatalf("LoadIgnoreList(%q, file=%q): %v", iflag, fileContent, err)
	}
	return append([]string{}, IgnoreFileList...)
}

func has(list []string, s string) bool { return slices.Contains(list, s) }

// TestIgnoreDefaults 无 -i/文件 → 强制项 + 默认项都在
func TestIgnoreDefaults(t *testing.T) {
	got := loadIgnore(t, "", "")
	for _, want := range []string{".local-mirror", ".git", ".DS_Store"} {
		if !has(got, want) {
			t.Errorf("default list missing %q: %v", want, got)
		}
	}
}

// TestIgnoreAdd -i 追加的普通模式与默认项共存
func TestIgnoreAdd(t *testing.T) {
	got := loadIgnore(t, "node_modules,dist", "")
	for _, want := range []string{".local-mirror", ".git", ".DS_Store", "node_modules", "dist"} {
		if !has(got, want) {
			t.Errorf("missing %q: %v", want, got)
		}
	}
}

// TestIgnoreNegateDefault !.git 取消默认项 .git，其余默认项保留
func TestIgnoreNegateDefault(t *testing.T) {
	got := loadIgnore(t, "!.git", "")
	if has(got, ".git") {
		t.Errorf("!.git should remove .git: %v", got)
	}
	if !has(got, ".local-mirror") || !has(got, ".DS_Store") {
		t.Errorf("!.git must not affect other defaults: %v", got)
	}
}

// TestIgnoreNegateAndAdd 取消一个默认项 + 追加普通项，可同一次给
func TestIgnoreNegateAndAdd(t *testing.T) {
	got := loadIgnore(t, "node_modules,!.DS_Store", "")
	if has(got, ".DS_Store") {
		t.Errorf("!.DS_Store should remove it: %v", got)
	}
	if !has(got, "node_modules") || !has(got, ".git") || !has(got, ".local-mirror") {
		t.Errorf("add + other defaults must remain: %v", got)
	}
}

// TestIgnoreNegateForced !.local-mirror 被拒绝（强制项不可取消）
func TestIgnoreNegateForced(t *testing.T) {
	oldFlag, oldList := *Ignore, IgnoreFileList
	t.Cleanup(func() { *Ignore = oldFlag; IgnoreFileList = oldList })
	*Ignore = "!.local-mirror"
	if err := LoadIgnoreList(t.TempDir()); err == nil {
		t.Fatal("un-ignoring the forced .local-mirror should error")
	}
}

// TestIgnoreFileNegation ignore 文件里的 !pattern 同样生效，# 注释跳过
func TestIgnoreFileNegation(t *testing.T) {
	got := loadIgnore(t, "", "!.git\n# a comment\nbuild\n")
	if has(got, ".git") {
		t.Errorf(".git should be removed via ignore file: %v", got)
	}
	if !has(got, "build") {
		t.Errorf("build should be added via ignore file: %v", got)
	}
}

// TestIgnoreInvalidPattern 非法 glob 仍被 filepath.Match 预校验拦下
func TestIgnoreInvalidPattern(t *testing.T) {
	oldFlag, oldList := *Ignore, IgnoreFileList
	t.Cleanup(func() { *Ignore = oldFlag; IgnoreFileList = oldList })
	*Ignore = "[unclosed"
	if err := LoadIgnoreList(t.TempDir()); err == nil {
		t.Fatal("invalid glob pattern should error")
	}
}
