package tree

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"local-mirror/config"
)

// 粗精度 mtime：刚改完就算的哈希不能凭 size+mtime 复用。模拟 FAT——同尺寸改写后
// mtime 落在同一刻度（这里直接设回原值），校准必须重算出新内容的哈希
func TestBuildFileTreeRehashesRacyFiles(t *testing.T) {
	root := t.TempDir()
	config.StartPath = root
	config.IgnoreFileList = []string{".local-mirror"}
	p := filepath.Join(root, "f.txt")
	if err := os.WriteFile(p, []byte("aaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Truncate(2 * time.Second)
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	InitDB()
	defer DB.Close()
	if err := BuildFileTree(root); err != nil {
		t.Fatal(err)
	}
	n0, err := GetNodeByPath("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !n0.HashRacy {
		t.Fatal("hash computed within the mtime granularity was not marked racy")
	}

	if err := os.WriteFile(p, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := BuildFileTree(root); err != nil {
		t.Fatal(err)
	}
	n1, err := GetNodeByPath("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if n1.Hash == n0.Hash {
		t.Error("same-size rewrite within the mtime tick kept the stale hash")
	}
}

// 远离 mtime 算的哈希照常复用，不能因为这次修复把启动校准变成全量重哈希
func TestRacyHashWindow(t *testing.T) {
	now := time.Now()
	if RacyHash(now.Add(-time.Hour), now) {
		t.Error("an hour-old mtime marked racy")
	}
	if !RacyHash(now.Add(-time.Second), now) {
		t.Error("a 1s-old mtime not marked racy")
	}
	if !RacyHash(now.Add(time.Minute), now) {
		t.Error("a future mtime not marked racy")
	}
}
