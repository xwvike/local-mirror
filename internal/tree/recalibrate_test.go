package tree

import (
	"os"
	"path/filepath"
	"testing"

	"local-mirror/config"
)

// TestBuildFileTreeRecalibratesLocalDrift 验证 COR-01 所依赖的机制：重新运行 BuildFileTree
// 会按磁盘现状校准本地树——被外部删除的文件从树中剔除、被修改的文件哈希更新、未变的保留。
// 纯汇端没有 watcher，fullScan 正是靠这次重建纠正运行期的本地漂移。
func TestBuildFileTreeRecalibratesLocalDrift(t *testing.T) {
	root := t.TempDir()
	config.StartPath = root
	config.IgnoreFileList = []string{".local-mirror"}

	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("keep.txt", "keep")
	write("gone.txt", "will be deleted")
	write("changed.txt", "v1")

	InitDB()
	defer DB.Close()
	if err := BuildFileTree(root); err != nil {
		t.Fatalf("BuildFileTree: %v", err)
	}
	n0, err := GetNodeByPath("changed.txt")
	if err != nil {
		t.Fatalf("GetNodeByPath: %v", err)
	}
	h0 := n0.Hash

	// 模拟汇端本地漂移：删一个、改一个（长度变化 → size 变 → 校准必重算哈希，
	// 不依赖粗粒度 mtime）
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	write("changed.txt", "v2-with-different-length")

	// fullScan 现在对纯汇端做的事：按磁盘重建
	if err := BuildFileTree(root); err != nil {
		t.Fatalf("re-calibrate BuildFileTree: %v", err)
	}

	if ok, _ := HasPath("gone.txt"); ok {
		t.Error("被删除的 gone.txt 应在重建后从树中剔除")
	}
	n1, err := GetNodeByPath("changed.txt")
	if err != nil {
		t.Fatalf("GetNodeByPath after recalibrate: %v", err)
	}
	if n1.Hash == h0 {
		t.Error("被修改的 changed.txt 哈希应在重建后更新")
	}
	if ok, _ := HasPath("keep.txt"); !ok {
		t.Error("未变的 keep.txt 应仍在树中")
	}
}
