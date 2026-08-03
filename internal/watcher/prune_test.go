package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"local-mirror/config"

	"github.com/fsnotify/fsnotify"
)

// TestPerformScanPrunesMissingDir performScan 遇到物理已消失的目录时，
// 必须把它从 heatMap 剔除，否则孤儿条目每轮重新分层、tier2 反复扫描并刷屏日志
// （复现 ~/Project/Applications 软链删除后遗留 1200 条幽灵目录、日志涨到 1.9GB 的缺陷）
func TestPerformScanPrunesMissingDir(t *testing.T) {
	root := t.TempDir()
	config.StartPath = root

	// live 真实存在，ghost 只在 heatMap 里、磁盘上没有
	if err := os.MkdirAll(filepath.Join(root, "live"), 0755); err != nil {
		t.Fatal(err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sw := &ScoreWatch{
		Watcher:    watcher,
		tier1Limit: 1024,
		heatMap: map[string]*HeatScore{
			"live":  {Path: "live", Score: 50},
			"ghost": {Path: "ghost", Score: 50},
		},
		ctx:    ctx,
		cancel: cancel,
	}

	sw.performScan()

	if _, ok := sw.heatMap["ghost"]; ok {
		t.Error("物理已消失的 ghost 目录应从 heatMap 剔除")
	}
	if _, ok := sw.heatMap["live"]; !ok {
		t.Error("真实存在的 live 目录不应被误删")
	}
}
