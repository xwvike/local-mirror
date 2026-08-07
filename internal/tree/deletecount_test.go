package tree

import (
	"os"
	"path/filepath"
	"testing"

	"local-mirror/config"
)

// TestDeleteNodesCountsAfterDedup 验证 DB-01：删除批次同时含父目录与其子节点时，
// 元数据计数按「去重后的唯一节点」计算，而非把子树重复计数。
//
// 复现旧缺陷：批次 [d, d/a.txt] 会让 d 的整棵子树（d + a.txt + b.txt）被遍历计数一次，
// 再叠加单独传入的 d/a.txt 又数一次 a.txt → totalFile=3，超过实际 file_count=2，
// 更新逻辑 oldFileCount >= totalFileCount 不成立而整段跳过，file_count 永久卡在陈旧值。
func TestDeleteNodesCountsAfterDedup(t *testing.T) {
	root := t.TempDir()
	config.StartPath = root
	// 忽略 .local-mirror，否则 InitDB 建的状态目录会混进计数基线
	config.IgnoreFileList = []string{".local-mirror"}

	for _, name := range []string{"a.txt", "b.txt"} {
		p := filepath.Join(root, "d", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	InitDB()
	defer DB.Close()
	if err := BuildFileTree(root); err != nil {
		t.Fatalf("BuildFileTree: %v", err)
	}

	// 基线：dirs = 根(.) + d = 2；files = a.txt + b.txt = 2
	if got, _ := GetMeta("dir_count"); got != 2 {
		t.Fatalf("前置：dir_count 基线应为 2，实际 %d", got)
	}
	if got, _ := GetMeta("file_count"); got != 2 {
		t.Fatalf("前置：file_count 基线应为 2，实际 %d", got)
	}

	// 批次同传父目录与其中一个子文件——去重后应删除 {d, a.txt, b.txt}
	if err := DeleteNodes([]string{"d", filepath.Join("d", "a.txt")}); err != nil {
		t.Fatalf("DeleteNodes: %v", err)
	}

	// 期望：dir_count 减 1（d，根仍在）→ 1；file_count 减 2（a.txt+b.txt）→ 0。
	// 旧缺陷下 file_count 会停在 2（减过头被跳过）。
	if got, _ := GetMeta("dir_count"); got != 1 {
		t.Errorf("dir_count 应为 1，实际 %d", got)
	}
	if got, _ := GetMeta("file_count"); got != 0 {
		t.Errorf("file_count 应为 0（去重后精确减 2），实际 %d —— 子树被重复计数了", got)
	}
}
