package tree

import (
	"os"
	"path/filepath"
	"testing"

	"local-mirror/config"
)

// TestDeleteNodesTolerantOfMissingPaths 删一个从没进过树的路径必须是无副作用的
// no-op。短命文件（编辑器原子保存的临时文件、git 的 index.lock/HEAD.lock）在
// 防抖窗口内建了又删，落库前就已消失，删除批次里自然找不到它们——要删的东西
// 本就不在，正是想要的结果，不该按异常处理，更不能影响同批次里真实存在的节点
func TestDeleteNodesTolerantOfMissingPaths(t *testing.T) {
	root := t.TempDir()
	config.StartPath = root
	config.IgnoreFileList = nil

	for _, name := range []string{"keep.txt", "remove.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	InitDB()
	defer DB.Close()

	if err := BuildFileTree(root); err != nil {
		t.Fatalf("BuildFileTree 失败: %v", err)
	}

	// 一批里混入从未入树的路径：git 锁文件、编辑器临时文件、以及一个真实节点
	batch := []string{
		"never-existed/.git/index.lock",
		"foo.txt.tmp.12345.abcdef",
		"remove.txt",
		"also-missing.txt",
	}
	if err := DeleteNodes(batch); err != nil {
		t.Fatalf("批次含不存在的路径时不该报错: %v", err)
	}

	// 真实节点确实被删掉
	if ok, _ := HasPath("remove.txt"); ok {
		t.Error("remove.txt 应已被删除")
	}
	// 同批次里的其他节点毫发无损
	if ok, err := HasPath("keep.txt"); err != nil || !ok {
		t.Errorf("keep.txt 不该受影响 (ok=%v err=%v)", ok, err)
	}

	// 纯不存在路径的批次同样是干净的 no-op
	if err := DeleteNodes([]string{"nothing/here.txt"}); err != nil {
		t.Fatalf("全是不存在路径时不该报错: %v", err)
	}
	if ok, err := HasPath("keep.txt"); err != nil || !ok {
		t.Errorf("空操作后 keep.txt 仍应在树中 (ok=%v err=%v)", ok, err)
	}
}
