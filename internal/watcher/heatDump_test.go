package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"local-mirror/config"
)

// newTestSW 构造一个只填了热度字段的 ScoreWatch（WriteHeatJSON 只用这些）
func newTestSW(tier1Limit int, tier1Paths []string, all map[string]*HeatScore) *ScoreWatch {
	sw := &ScoreWatch{heatMap: all, tier1Limit: tier1Limit}
	for _, p := range tier1Paths {
		sw.tier1 = append(sw.tier1, all[p])
	}
	return sw
}

// TestHeatJSONRoundTrip 落盘 + 读回：按分数降序、tier 标注正确、原子写无残留
func TestHeatJSONRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".local-mirror"), 0755); err != nil {
		t.Fatal(err)
	}
	config.StartPath = root

	all := map[string]*HeatScore{
		"src":        {Path: "src", Score: 42.0, EventCount: 100},
		"assets/img": {Path: "assets/img", Score: 128.5, EventCount: 3410},
		"docs":       {Path: "docs", Score: 3.2, EventCount: 12},
	}
	sw := newTestSW(4, []string{"assets/img", "src"}, all)
	sw.WriteHeatJSON()

	// 原子写：不残留 .tmp
	if _, err := os.Stat(filepath.Join(root, ".local-mirror", "heat.json.tmp")); !os.IsNotExist(err) {
		t.Error("temp file lingered after atomic write")
	}

	snap, err := LoadHeat(root)
	if err != nil || snap == nil {
		t.Fatalf("LoadHeat: (%v, %v)", snap, err)
	}
	if snap.Schema != HeatSchemaVersion || snap.Total != 3 || snap.Tier1Count != 2 || snap.Tier1Limit != 4 {
		t.Fatalf("header wrong: %+v", snap)
	}
	// 降序：assets/img > src > docs
	if snap.Entries[0].Path != "assets/img" || snap.Entries[1].Path != "src" || snap.Entries[2].Path != "docs" {
		t.Fatalf("not sorted by score desc: %+v", snap.Entries)
	}
	// tier 标注：tier1 集合内为 1，其余 2
	if snap.Entries[0].Tier != 1 || snap.Entries[1].Tier != 1 || snap.Entries[2].Tier != 2 {
		t.Fatalf("tier labels wrong: %+v", snap.Entries)
	}
	if snap.Entries[0].Events != 3410 {
		t.Fatalf("events not carried: %d", snap.Entries[0].Events)
	}
}

// TestLoadHeatMissing 无 heat.json → (nil, nil)，供 --heat 报「无热度表」
func TestLoadHeatMissing(t *testing.T) {
	if s, err := LoadHeat(t.TempDir()); err != nil || s != nil {
		t.Fatalf("missing heat should be (nil, nil), got (%v, %v)", s, err)
	}
}

// TestHeatStale 新写不陈旧，人为回拨 generated_unix 则陈旧
func TestHeatStale(t *testing.T) {
	fresh := &HeatSnapshot{GeneratedUnix: time.Now().Unix()}
	if fresh.Stale() {
		t.Error("fresh heat snapshot should not be stale")
	}
	old := &HeatSnapshot{GeneratedUnix: time.Now().Add(-5 * time.Minute).Unix()}
	if !old.Stale() {
		t.Error("5-minute-old heat snapshot should be stale")
	}
}
