package tree

import (
	"fmt"
	"local-mirror/config"
	"path/filepath"
	"sort"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// 一批写入在 Commit 前不入库，Commit 后与逐条立即写入的结果一致
func TestBatchCommit(t *testing.T) {
	config.StartPath = t.TempDir()
	InitDB()
	defer DB.Close()

	j := filepath.Join
	if err := AddNodes([]*Node{
		{ID: "root", Path: ".", IsDir: true},
		{ID: "xid", Path: "x", Name: "x", ParentID: "root", IsDir: true},
		{ID: "yid", Path: "y", Name: "y", ParentID: "root", IsDir: true},
		{ID: "yold", Path: j("y", "old"), Name: "old", ParentID: "yid"},
		{ID: "zid", Path: "z", Name: "z", ParentID: "root", IsDir: true},
		{ID: "zk", Path: j("z", "k"), Name: "k", ParentID: "zid"},
	}); err != nil {
		t.Fatal(err)
	}

	var b Batch
	b.Add(&Node{ID: "n1", Path: "a", Name: "a", ParentID: "root", IsDir: true}, false)
	b.Add(&Node{ID: "n2", Path: j("a", "f1"), Name: "f1", ParentID: "n1"}, true)
	// 已有目录的更新带着新 ID 入队，其下新节点引用的是这个新 ID：提交时须按路径解析回原 ID
	b.Add(&Node{ID: "tmp-x", Path: "x", Name: "x", ParentID: "root", IsDir: true}, false)
	b.Add(&Node{ID: "n3", Path: j("x", "c"), Name: "c", ParentID: "tmp-x"}, true)
	// 类型互换：删掉 y 整棵子树后重建同名目录
	b.Delete("y")
	b.Add(&Node{ID: "n4", Path: "y", Name: "y", ParentID: "root", IsDir: true}, false)
	// 已有目录仅更新自身（chmod），原有子节点不受影响
	b.Add(&Node{ID: "tmp-z", Path: "z", Name: "z", ParentID: "root", IsDir: true}, false)

	if ok, _ := HasPath("a"); ok {
		t.Fatal("queued writes reached the database before Commit")
	}
	b.Commit()

	want := map[string][]string{
		".": {"a", "x", "y", "z"},
		"a": {j("a", "f1")},
		"x": {j("x", "c")},
		"y": {},
		"z": {j("z", "k")},
	}
	for dir, exp := range want {
		got, err := GetDirContents(dir)
		if err != nil {
			t.Fatalf("listing %s: %v", dir, err)
		}
		if p := paths(got); !equal(p, exp) {
			t.Errorf("%s lists %v, want %v", dir, p, exp)
		}
	}
	if ok, _ := HasPath(j("y", "old")); ok {
		t.Error("deleted subtree survived the re-created directory")
	}
	if n, err := GetNodeByPath("x"); err != nil || n.ID != "xid" {
		t.Errorf("updating x must keep its ID, got %+v %v", n, err)
	}
}

// 一条写不进去的记录（父目录的 children 数据损坏）只能丢它自己，同批其余写入照常入库
func TestBatchFailureIsIsolated(t *testing.T) {
	config.StartPath = t.TempDir()
	InitDB()
	defer DB.Close()

	if err := AddNodes([]*Node{
		{ID: "root", Path: ".", IsDir: true},
		{ID: "bad", Path: "bad", Name: "bad", ParentID: "root", IsDir: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := DB.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("children")).Put([]byte("bad"), []byte("{not json"))
	}); err != nil {
		t.Fatal(err)
	}

	var b Batch
	b.Add(&Node{ID: "g1", Path: "good", Name: "good", ParentID: "root", IsDir: true}, false)
	b.Add(&Node{ID: "p1", Path: filepath.Join("bad", "x"), Name: "x", ParentID: "bad"}, true)
	b.Add(&Node{ID: "g2", Path: filepath.Join("good", "f"), Name: "f", ParentID: "g1"}, true)
	b.Commit()

	for _, p := range []string{"good", filepath.Join("good", "f")} {
		if ok, _ := HasPath(p); !ok {
			t.Errorf("%s was dropped along with the failing write", p)
		}
	}
}

// 大目录分批提交：攒满上限即入库，不等目录处理完
func TestBatchCommitsWhenFull(t *testing.T) {
	config.StartPath = t.TempDir()
	InitDB()
	defer DB.Close()
	if err := AddNodes([]*Node{{ID: "root", Path: ".", IsDir: true}}); err != nil {
		t.Fatal(err)
	}

	var b Batch
	for i := range batchMaxOps {
		name := fmt.Sprintf("f%d", i)
		b.Add(&Node{ID: name, Path: name, Name: name, ParentID: "root"}, true)
	}
	if ok, _ := HasPath(fmt.Sprintf("f%d", batchMaxOps-1)); !ok {
		t.Errorf("a full batch of %d writes was not committed", batchMaxOps)
	}
	if len(b.ops) != 0 {
		t.Errorf("%d writes left queued after the automatic commit", len(b.ops))
	}
}

// 写盘顺序是批量提交的正确性前提：本批含文件数据时，数据必须在数据库提交之前落盘，
// 否则崩溃后库里会记着内容并未落盘的文件；只有目录等元数据时不必付这次落盘
func TestBatchFlushesDataBeforeCommit(t *testing.T) {
	config.StartPath = t.TempDir()
	InitDB()
	defer DB.Close()
	if err := AddNodes([]*Node{{ID: "root", Path: ".", IsDir: true}}); err != nil {
		t.Fatal(err)
	}

	saved := flushData
	defer func() { flushData = saved }()
	calls := 0
	flushData = func() error {
		calls++
		if ok, _ := HasPath("f"); ok {
			t.Error("the file node was committed before its data was flushed")
		}
		return nil
	}

	var b Batch
	b.Add(&Node{ID: "d", Path: "d", Name: "d", ParentID: "root", IsDir: true}, false)
	b.Commit()
	if calls != 0 {
		t.Errorf("a batch without file data flushed data %d times", calls)
	}
	b.Add(&Node{ID: "f", Path: "f", Name: "f", ParentID: "root"}, true)
	b.Commit()
	if calls != 1 {
		t.Errorf("a batch with file data flushed data %d times, want 1", calls)
	}
	if ok, _ := HasPath("f"); !ok {
		t.Error("the file node was not committed")
	}
}

// 中继下游被变更日志唤醒后立刻来读树，所以变更目录只能在本批提交之后登记
func TestBatchRecordsChangedDirsAfterCommit(t *testing.T) {
	config.StartPath = t.TempDir()
	InitDB()
	defer DB.Close()
	if err := AddNodes([]*Node{{ID: "root", Path: ".", IsDir: true}}); err != nil {
		t.Fatal(err)
	}
	const dir = "relay-changed"
	pending := func() bool {
		mu.Lock()
		defer mu.Unlock()
		_, ok := recentChangedDirs[dir]
		return ok
	}
	// 不让 2 秒后的节流落库写进别的测试打开的库
	defer func() {
		mu.Lock()
		delete(recentChangedDirs, dir)
		mu.Unlock()
	}()

	var b Batch
	b.Add(&Node{ID: "n", Path: filepath.Join(dir, "x"), Name: "x", ParentID: "root"}, false)
	b.ChangedDir(dir)
	if pending() {
		t.Fatal("the changed directory was recorded before the batch was committed")
	}
	b.Commit()
	if !pending() {
		t.Error("the changed directory was not recorded after the commit")
	}
}

func paths(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Path)
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
