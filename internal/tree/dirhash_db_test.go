package tree

import (
	"os"
	"path/filepath"
	"testing"

	"local-mirror/config"
)

// TestDirHashesThroughDB 验证 DirHashes 经真实 bbolt 树库 + generation 记忆化的完整链路：
//   - 从建好的树能算出各目录 rollup；
//   - 一次树变更（DeleteNode）后，generation 自增使缓存失效，rollup 随之更新；
//   - 变更沿祖先链传播，无关子树不受影响。
func TestDirHashesThroughDB(t *testing.T) {
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
	write("a/f1.txt", "one")
	write("a/b/f2.txt", "two")
	write("c/f3.txt", "three")

	InitDB()
	defer DB.Close()
	if err := BuildFileTree(root); err != nil {
		t.Fatalf("BuildFileTree: %v", err)
	}

	h1, err := DirHashes()
	if err != nil {
		t.Fatalf("DirHashes: %v", err)
	}
	for _, dir := range []string{".", "a", "a/b", "c"} {
		if h1[dir] == "" {
			t.Fatalf("dir %q missing rollup after build", dir)
		}
	}

	// 记忆化：树没变，再调一次应返回一致结果
	h1b, _ := DirHashes()
	if len(h1b) != len(h1) || h1b["."] != h1["."] {
		t.Fatal("memoized DirHashes returned inconsistent map")
	}

	// 删掉 a/b/f2.txt 的节点 → generation 自增 → 缓存失效
	aOld, cOld := h1["a"], h1["c"]
	abOld := h1["a/b"]
	if err := DeleteNode("a/b/f2.txt"); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	h2, err := DirHashes()
	if err != nil {
		t.Fatalf("DirHashes after delete: %v", err)
	}

	// 祖先链 a/b、a、. 的 rollup 必须变
	if h2["a/b"] == abOld {
		t.Error("a/b rollup unchanged after deleting its child file")
	}
	if h2["a"] == aOld {
		t.Error("a rollup did not propagate child deletion")
	}
	if h2["."] == h1["."] {
		t.Error("root rollup did not propagate deep deletion")
	}
	// 无关子树 c 不受影响
	if h2["c"] != cOld {
		t.Error("unrelated subtree c wrongly changed")
	}
}
