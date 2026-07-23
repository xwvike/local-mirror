package watcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"local-mirror/config"

	log "github.com/sirupsen/logrus"
)

// HeatSchemaVersion heat.json 的结构版本，读端据此容错跨版本字段变化
const HeatSchemaVersion = 1

// heatStaleAfter 读端判定 heat.json 陈旧的阈值。heat.json 只在被观测时随
// status.json 一起刷新（见 status 的观测门），观测中超过此时长没更新即认为
// 源端已停
const heatStaleAfter = 15 * time.Second

// HeatEntry heat.json 里的单个目录条目
type HeatEntry struct {
	Path   string  `json:"path"`
	Score  float64 `json:"score"`
	Tier   int     `json:"tier"` // 1 = 实时 watch，2 = 惰性轮询
	Events int     `json:"events"`
}

// HeatSnapshot 源侧常驻进程周期写下的目录热度快照（读端 --heat 渲染）。
// 与 status.json 同属可弃状态：删了下次自动重建，不影响同步
type HeatSnapshot struct {
	Schema        int         `json:"schema"`
	GeneratedUnix int64       `json:"generated_unix"`
	Tier1Limit    int         `json:"tier1_limit"`
	Tier1Count    int         `json:"tier1_count"`
	Total         int         `json:"total"`
	Entries       []HeatEntry `json:"entries"` // 按分数降序
}

// WriteHeatJSON 原子落盘当前热度表：同目录临时文件 + rename，避免读端读到
// 半个 JSON。条目按分数降序
func (sw *ScoreWatch) WriteHeatJSON() {
	sw.mu.Lock()
	entries := make([]HeatEntry, 0, len(sw.heatMap))
	tier1 := make(map[string]struct{}, len(sw.tier1))
	for _, h := range sw.tier1 {
		tier1[h.Path] = struct{}{}
	}
	for _, h := range sw.heatMap {
		tier := 2
		if _, ok := tier1[h.Path]; ok {
			tier = 1
		}
		entries = append(entries, HeatEntry{Path: h.Path, Score: h.Score, Tier: tier, Events: h.EventCount})
	}
	tier1Limit := sw.tier1Limit
	sw.mu.Unlock()

	sort.Slice(entries, func(i, j int) bool { return entries[i].Score > entries[j].Score })

	snap := HeatSnapshot{
		Schema:        HeatSchemaVersion,
		GeneratedUnix: time.Now().Unix(),
		Tier1Limit:    tier1Limit,
		Tier1Count:    len(tier1),
		Total:         len(entries),
		Entries:       entries,
	}
	data, err := json.MarshalIndent(&snap, "", "  ")
	if err != nil {
		return
	}
	p := filepath.Join(config.StartPath, ".local-mirror", "heat.json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Errorf("failed to write heat snapshot: %v", err)
		return
	}
	_ = os.Rename(tmp, p)
}

// LoadHeat 读取并解析 heat.json（供 --heat 子命令）。
// 文件不存在返回 (nil, nil)：调用方据此报「无热度表」
func LoadHeat(root string) (*HeatSnapshot, error) {
	data, err := os.ReadFile(filepath.Join(root, ".local-mirror", "heat.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s HeatSnapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Stale 快照是否已陈旧（被观测中仍长时间未更新 = 源进程可能已停）
func (s *HeatSnapshot) Stale() bool {
	return time.Since(time.Unix(s.GeneratedUnix, 0)) > heatStaleAfter
}
