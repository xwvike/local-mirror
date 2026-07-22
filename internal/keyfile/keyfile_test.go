package keyfile

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenerate 验证生成的 key 是 base64(32B)、文件权限 600、Load 可读回
func TestGenerate(t *testing.T) {
	root := t.TempDir()
	key, err := Generate(root, false)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		t.Fatalf("key is not valid base64: %v", err)
	}
	if len(raw) != KeyBytes {
		t.Fatalf("key decodes to %d bytes, want %d", len(raw), KeyBytes)
	}
	info, err := os.Stat(Path(root))
	if err != nil {
		t.Fatalf("key file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("key file permissions %o, want 600", perm)
	}
	loaded, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded != key {
		t.Fatalf("Load returned %q, want %q", loaded, key)
	}
}

// TestGenerateRefusesOverwrite 断链保护：已有文件时拒绝，--force 才重写
func TestGenerateRefusesOverwrite(t *testing.T) {
	root := t.TempDir()
	first, err := Generate(root, false)
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	if _, err := Generate(root, false); err == nil {
		t.Fatal("second Generate without force should refuse")
	} else if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("refusal should mention --force, got: %v", err)
	}
	second, err := Generate(root, true)
	if err != nil {
		t.Fatalf("Generate with force: %v", err)
	}
	if second == first {
		t.Fatal("forced regeneration returned the same key")
	}
}

// TestLoadMissingAndEmpty 文件不存在 = ("", nil)；空文件 = 损坏报错
func TestLoadMissingAndEmpty(t *testing.T) {
	root := t.TempDir()
	key, err := Load(root)
	if err != nil || key != "" {
		t.Fatalf("missing file should be (\"\", nil), got (%q, %v)", key, err)
	}
	if err := os.MkdirAll(filepath.Dir(Path(root)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(root), []byte("  \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil {
		t.Fatal("empty key file should be an error")
	}
}

// TestSave 拨号端对称落盘：首次写入、内容一致跳过、不同内容覆盖、权限 600
func TestSave(t *testing.T) {
	root := t.TempDir()
	written, err := Save(root, "first-key")
	if err != nil || !written {
		t.Fatalf("first Save should write, got (%v, %v)", written, err)
	}
	written, err = Save(root, "first-key")
	if err != nil || written {
		t.Fatalf("identical Save should skip, got (%v, %v)", written, err)
	}
	written, err = Save(root, "second-key")
	if err != nil || !written {
		t.Fatalf("changed Save should rewrite, got (%v, %v)", written, err)
	}
	loaded, err := Load(root)
	if err != nil || loaded != "second-key" {
		t.Fatalf("Load after Save = (%q, %v), want second-key", loaded, err)
	}
	info, _ := os.Stat(Path(root))
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("saved key file permissions %o, want 600", perm)
	}
}

// TestSaveHealsCorruptFile 损坏（空）文件不阻塞 Save，直接覆盖自愈
func TestSaveHealsCorruptFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(Path(root)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(root), []byte("\n"), 0600); err != nil {
		t.Fatal(err)
	}
	written, err := Save(root, "healed")
	if err != nil || !written {
		t.Fatalf("Save over corrupt file should write, got (%v, %v)", written, err)
	}
}

// TestFingerprint 指纹确定、8 位十六进制、不同 key 不同指纹
func TestFingerprint(t *testing.T) {
	a, b := Fingerprint("key-a"), Fingerprint("key-b")
	if a != Fingerprint("key-a") {
		t.Fatal("fingerprint is not deterministic")
	}
	if len(a) != 8 {
		t.Fatalf("fingerprint length %d, want 8 hex chars", len(a))
	}
	if a == b {
		t.Fatal("different keys produced the same fingerprint")
	}
}
