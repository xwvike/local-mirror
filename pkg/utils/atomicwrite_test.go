package utils

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(p, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Fatalf("content = %q, want new", b)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Errorf("perm = %o, want 600 (must not inherit the old file's 644)", fi.Mode().Perm())
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".config.yml.tmp-*")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

// 写入失败时原文件必须原封不动（os.WriteFile 会先截断）
func TestWriteFileAtomicKeepsOriginalOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("relies on a read-only directory")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "key")
	if err := os.WriteFile(p, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { // 目录只读：临时文件建不出来
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if err := WriteFileAtomic(p, []byte("replacement"), 0o600); err == nil {
		t.Fatal("expected an error writing into a read-only directory")
	}
	if b, _ := os.ReadFile(p); string(b) != "original" {
		t.Fatalf("original content damaged: %q", b)
	}
}
