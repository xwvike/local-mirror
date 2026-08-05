package tree

import (
	"encoding/binary"
	"encoding/json"
	"slices"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"local-mirror/config"
)

// putChangedDirsAt 绕过 addChangedDir 直接以指定时刻写入记录，
// 用于构造"清理没跑过、陈旧记录还在库里"的状态
func putChangedDirsAt(t *testing.T, at time.Time, paths []string) {
	t.Helper()
	err := DB.Update(func(tx *bolt.Tx) error {
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, uint64(at.Unix()))
		data, err := json.Marshal(paths)
		if err != nil {
			return err
		}
		return tx.Bucket([]byte("changed_dirs")).Put(key, data)
	})
	if err != nil {
		t.Fatalf("写入 changed_dirs 失败: %v", err)
	}
}

// TestGetChangedDirsClampsToRetention 读取侧必须自己守住保留窗口。
// 清理只发生在写入事务里，源端空闲时不再有写入，过期记录会永久滞留；
// 客户端每做一次全量扫描就把游标归 0，若读取侧照单全收，就会把整桶陈旧
// 路径反复重放——其中已被删除的目录每轮报一次 not found，通宵刷屏
func TestGetChangedDirsClampsToRetention(t *testing.T) {
	config.StartPath = t.TempDir()
	InitDB()
	defer DB.Close()

	now := time.Now()
	putChangedDirsAt(t, now.Add(-3*time.Hour), []string{"stale/long-deleted"})
	putChangedDirsAt(t, now.Add(-ChangedDirRetention-time.Minute), []string{"stale/just-past-horizon"})
	putChangedDirsAt(t, now.Add(-10*time.Minute), []string{"fresh/still-relevant"})

	// 游标归 0 是全量扫描后的常态，正是触发重放的那个入口
	dirs, err := GetChangedDirs(0, now.Unix())
	if err != nil {
		t.Fatalf("GetChangedDirs 失败: %v", err)
	}
	if !slices.Contains(dirs, "fresh/still-relevant") {
		t.Errorf("保留窗口内的记录必须返回, got: %v", dirs)
	}
	for _, stale := range []string{"stale/long-deleted", "stale/just-past-horizon"} {
		if slices.Contains(dirs, stale) {
			t.Errorf("超出保留窗口的记录不该返回: %s (got: %v)", stale, dirs)
		}
	}
}

// TestGetChangedDirsKeepsCallerStartWhenNewer 抬下沿只能收窄不能放宽：
// 调用方给的游标比下沿新时必须原样生效，否则会把已同步过的区间重发一遍
func TestGetChangedDirsKeepsCallerStartWhenNewer(t *testing.T) {
	config.StartPath = t.TempDir()
	InitDB()
	defer DB.Close()

	now := time.Now()
	putChangedDirsAt(t, now.Add(-30*time.Minute), []string{"already/synced"})
	putChangedDirsAt(t, now.Add(-2*time.Minute), []string{"after/cursor"})

	dirs, err := GetChangedDirs(now.Add(-5*time.Minute).Unix(), now.Unix())
	if err != nil {
		t.Fatalf("GetChangedDirs 失败: %v", err)
	}
	if slices.Contains(dirs, "already/synced") {
		t.Errorf("游标之前的记录不该返回, got: %v", dirs)
	}
	if !slices.Contains(dirs, "after/cursor") {
		t.Errorf("游标之后的记录必须返回, got: %v", dirs)
	}
}
