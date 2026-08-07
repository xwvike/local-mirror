package watcher

import (
	"testing"

	"github.com/fsnotify/fsnotify"

	"local-mirror/config"
	"local-mirror/internal/tree"
)

// TestAddHeatWatchFailureFallsBackToTier2 验证 COR-04：新目录注册 watch 失败时必须
// 降级进 tier2 轮询，而不是留在 tier1——留在 tier1 会让系统以为在实时监听、实则漏掉
// 该目录的全部事件，变更长期不进源端树、汇端也补救不了。
//
// 用磁盘上不存在的目录触发真实的 Add 失败（fsnotify 对不存在路径返回 ENOENT），
// 无需注入假 watcher。
func TestAddHeatWatchFailureFallsBackToTier2(t *testing.T) {
	config.StartPath = t.TempDir()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	sw := &ScoreWatch{Watcher: w, heatMap: make(map[string]*HeatScore), tier1Limit: 100}

	sw.addHeat("does/not/exist", &tree.Node{Path: "does/not/exist", Depth: 2})

	if len(sw.tier1) != 0 {
		t.Errorf("注册失败的目录不该进 tier1，实际 tier1 有 %d 项", len(sw.tier1))
	}
	if len(sw.tier2) != 1 || sw.tier2[0].Path != "does/not/exist" {
		t.Errorf("注册失败的目录应降级进 tier2，实际 tier2=%v", sw.tier2)
	}
	if _, ok := sw.heatMap["does/not/exist"]; !ok {
		t.Error("heatMap 仍应记录该目录（只是监听方式从实时降为轮询）")
	}
}
