package app

import (
	"os"
	"path/filepath"
	"testing"

	"local-mirror/config"
)

// TestRenameOptimizationGatedByAllowDelete 验证 COR-02 门控：--allow-delete 关闭时
// 不启用会移除旧路径的重命名优化，delete+create 两条 diff 原样保留（delete 稍后被跳过、
// create 正常下载），从而旧文件被保留、不违反「默认只同步不删」。
func TestRenameOptimizationGatedByAllowDelete(t *testing.T) {
	save := config.AllowDelete
	defer func() { config.AllowDelete = save }()
	off := false
	config.AllowDelete = &off

	diffs := []DiffResult{
		{Path: "a.txt", Action: "delete", IsDir: false, Hash: "h"},
		{Path: "b.txt", Action: "create", IsDir: false, Hash: "h"},
	}
	got := maybeDetectRenames(diffs)
	if len(got) != 2 {
		t.Fatalf("allow-delete 关闭时不该消化 rename 对，应原样保留 2 条，实际 %d 条: %+v", len(got), got)
	}
}

// TestApplyRenameRejectsDriftedLocalFile 验证 COR-02 的重验：rename 前若本地旧文件已漂移
// （磁盘内容与 DB 记录的哈希不符），必须放弃 rename 并返回错误——否则会把「错内容」搬到
// 新路径并登记成上游哈希，造成静默损坏。返回错误后由 detectRenames 回落到 delete+download。
func TestApplyRenameRejectsDriftedLocalFile(t *testing.T) {
	root := t.TempDir()
	config.StartPath = root

	oldRel := "old.txt"
	if err := os.WriteFile(filepath.Join(root, oldRel), []byte("REAL CONTENT ON DISK"), 0o644); err != nil {
		t.Fatal(err)
	}
	// oldDiff.Hash 故意与磁盘真实内容不符（模拟本地漂移、DB 未更新）
	oldDiff := DiffResult{Path: oldRel, Hash: "0000deadbeef", IsDir: false}
	newDiff := DiffResult{Path: "new.txt", Hash: "0000deadbeef", IsDir: false}

	if err := applyRename(oldDiff, newDiff); err == nil {
		t.Fatal("本地旧文件哈希与 DB 不符时 applyRename 应返回错误（放弃 rename）")
	}
	// 旧文件原封未动、新文件未产生
	if _, err := os.Stat(filepath.Join(root, oldRel)); err != nil {
		t.Error("漂移时旧文件不该被移动")
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); err == nil {
		t.Error("漂移时不该产生新文件")
	}
}
