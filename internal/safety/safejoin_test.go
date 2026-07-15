package safety

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestSafeJoin(t *testing.T) {
	root := filepath.Join("/srv", "sync")
	okCases := map[string]string{
		"":                root,
		".":               root,
		"file.txt":        filepath.Join(root, "file.txt"),
		"sub/deep/x.js":   filepath.Join(root, "sub/deep/x.js"),
		"a/../b":          filepath.Join(root, "b"), // 内部 .. 未逃逸
		"./nested/./file": filepath.Join(root, "nested/file"),
	}
	for rel, want := range okCases {
		got, err := SafeJoin(root, rel)
		if err != nil {
			t.Errorf("SafeJoin(%q) unexpected error: %v", rel, err)
			continue
		}
		if got != want {
			t.Errorf("SafeJoin(%q) = %q, want %q", rel, got, want)
		}
	}

	badCases := []string{
		"../etc/passwd",
		"../../../Users/foo/.zshrc",
		"sub/../../escape",
		"a/b/../../../out",
		"/etc/passwd",       // 绝对路径（Windows 上为 rooted，同样拒绝）
		"/srv/sync-sibling", // 前缀相似但非子路径
		`\etc\passwd`,       // 反斜杠 rooted（Windows 语义锚定盘根，统一拒绝）
	}
	if runtime.GOOS == "windows" {
		// 盘符形式仅 Windows 有意义；Unix 上 "C:\evil" 是合法文件名，不拒
		badCases = append(badCases, `C:\evil`, "C:/evil", "C:evil")
	}
	for _, rel := range badCases {
		if _, err := SafeJoin(root, rel); err == nil {
			t.Errorf("SafeJoin(%q) should have been rejected", rel)
		}
	}
}

// TestSafeJoinPrefixNotSubdir 确认"同前缀但不是子目录"被拒（经典 HasPrefix 陷阱）
func TestSafeJoinPrefixNotSubdir(t *testing.T) {
	// root=/srv/sync，目标清洗后 =/srv/sync-evil，字符串前缀匹配但不是子路径
	if _, err := SafeJoin("/srv/sync", "../sync-evil/x"); err == nil {
		t.Error("prefix-but-not-subdir path accepted")
	}
}
