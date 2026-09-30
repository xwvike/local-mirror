package tree

import (
	"local-mirror/config"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// 首次同步记录的初值决定会不会整树推送：新建的库尚未同步（pending）；旧版本留下的库
// 没有这条记录、数据已由逐文件同步建立，必须视为已完成（done），否则老用户一升级就整树重推。
// 已有的记录（如推送进行中）跨重启保留
func TestInitialSyncInitialState(t *testing.T) {
	config.StartPath = t.TempDir()
	InitDB()
	rec, err := LoadInitialSync()
	if err != nil || rec.State != InitialSyncPending {
		t.Fatalf("fresh database: %+v, %v; want pending", rec, err)
	}

	if err := SaveInitialSync(InitialSync{State: InitialSyncRunning, Cursor: "a/b"}); err != nil {
		t.Fatal(err)
	}
	DB.Close()
	InitDB()
	if rec, _ := LoadInitialSync(); rec.State != InitialSyncRunning || rec.Cursor != "a/b" {
		t.Errorf("record not kept across reopen: %+v", rec)
	}

	// 旧版本的库：删掉记录模拟之
	if err := DB.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("meta")).Delete([]byte(initialSyncKey))
	}); err != nil {
		t.Fatal(err)
	}
	DB.Close()
	InitDB()
	defer DB.Close()
	if rec, _ := LoadInitialSync(); rec.State != InitialSyncDone {
		t.Errorf("database from an older version: %+v, want done", rec)
	}
}

// 续推游标随节点在同一事务里提交：提交前记录不变，提交后游标与节点一并可见
func TestBatchCommitsInitialSyncWithNodes(t *testing.T) {
	config.StartPath = t.TempDir()
	InitDB()
	defer DB.Close()
	if err := AddNodes([]*Node{{ID: "root", Path: ".", IsDir: true}}); err != nil {
		t.Fatal(err)
	}

	var b Batch
	b.Add(&Node{ID: "f", Path: "f", Name: "f", ParentID: "root"}, true)
	b.SetInitialSync(InitialSync{State: InitialSyncRunning, Cursor: "f", Files: 1})
	if rec, _ := LoadInitialSync(); rec.Cursor != "" {
		t.Fatalf("progress recorded before the batch was committed: %+v", rec)
	}
	b.Commit()
	rec, _ := LoadInitialSync()
	if rec.State != InitialSyncRunning || rec.Cursor != "f" || rec.Files != 1 {
		t.Errorf("progress after commit: %+v", rec)
	}
	if ok, _ := HasPath("f"); !ok {
		t.Error("node committed without its progress, or vice versa")
	}
}
