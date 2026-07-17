package tree

import (
	"local-mirror/config"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// 孤儿节点修复：节点存在于 nodes/path_index 但不在父目录 children 列表中
// （历史缺陷可造成此状态）时，对同一路径的再次写入（重启校准、watcher
// 重哈希都会触发）必须重建父链接，使其重新出现在目录列表里
func TestAddNodesRepairsOrphanLinkage(t *testing.T) {
	config.StartPath = t.TempDir()
	InitDB()
	defer DB.Close()

	root := &Node{ID: "rootid", Path: ".", Name: "r", IsDir: true}
	dir := &Node{ID: "dirid", Path: "d", Name: "d", ParentID: "rootid", IsDir: true}
	file := &Node{ID: "fileid", Path: filepath.Join("d", "f.txt"), Name: "f.txt",
		ParentID: "dirid", Size: 3, Hash: "abc"}
	if err := AddNodes([]*Node{root, dir, file}); err != nil {
		t.Fatal(err)
	}

	// 人为制造孤儿：抹掉 d 的 children 记录
	if err := DB.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("children")).Delete([]byte("dirid"))
	}); err != nil {
		t.Fatal(err)
	}
	if c, err := GetDirContents("d"); err != nil || len(c) != 0 {
		t.Fatalf("前置条件不成立：孤儿应不可见, err=%v len=%d", err, len(c))
	}

	// 校准场景：同路径节点再次写入（携带新 ID 也应复用旧 ID 并修复链接）
	again := &Node{ID: "brand-new-id", Path: filepath.Join("d", "f.txt"), Name: "f.txt",
		ParentID: "dirid", Size: 3, Hash: "abc"}
	if err := AddNodes([]*Node{again}); err != nil {
		t.Fatal(err)
	}

	c, err := GetDirContents("d")
	if err != nil || len(c) != 1 {
		t.Fatalf("孤儿未被修复: err=%v len=%d", err, len(c))
	}
	if c[0].ID != "fileid" {
		t.Fatalf("应复用旧节点 ID, got %s", c[0].ID)
	}
	// 再写一次不应产生重复引用
	if err := AddNodes([]*Node{again}); err != nil {
		t.Fatal(err)
	}
	if c, _ := GetDirContents("d"); len(c) != 1 {
		t.Fatalf("children 出现重复引用: len=%d", len(c))
	}
}
