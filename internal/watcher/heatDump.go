package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"local-mirror/config"

	log "github.com/sirupsen/logrus"
)

// WriteHeatSnapshot 把当前热度表快照写到 <同步根>/.local-mirror/heat.txt，
// 按分数降序排列并标注所属层级。由 SIGUSR1 触发（见 cmd 的信号接线），
// 用于在生产环境随时观察哪些目录被判为热门（实时 watch）、哪些在冷轮询。
// watcher 未运行（mirror 模式）时无操作。
func WriteHeatSnapshot() {
	sw := GlobalScoreWatch
	if sw == nil {
		log.Warn("heat snapshot: watcher not running (only reality/relay have a heat table)")
		return
	}

	sw.mu.Lock()
	entries := make([]HeatScore, 0, len(sw.heatMap))
	for _, h := range sw.heatMap {
		entries = append(entries, *h)
	}
	tier1 := make(map[string]struct{}, len(sw.tier1))
	for _, h := range sw.tier1 {
		tier1[h.Path] = struct{}{}
	}
	tier1Limit := sw.tier1Limit
	sw.mu.Unlock()

	sort.Slice(entries, func(i, j int) bool { return entries[i].Score > entries[j].Score })

	outPath := filepath.Join(config.StartPath, ".local-mirror", "heat.txt")
	f, err := os.Create(outPath)
	if err != nil {
		log.Errorf("failed to write heat snapshot: %v", err)
		return
	}
	defer f.Close()

	t1 := len(tier1)
	fmt.Fprintf(f, "# local-mirror directory heat snapshot  %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(f, "# %d directories: tier1 (real-time watch) %d / limit %d, tier2 (lazy poll) %d\n",
		len(entries), t1, tier1Limit, len(entries)-t1)
	fmt.Fprintf(f, "# %-9s %-6s %-7s %s\n", "score", "tier", "events", "directory")
	for _, e := range entries {
		tier := "tier2"
		if _, ok := tier1[e.Path]; ok {
			tier = "tier1"
		}
		fmt.Fprintf(f, "%10.2f %-6s %8d %s\n", e.Score, tier, e.EventCount, e.Path)
	}
	log.Infof("heat snapshot written to %s (%d directories)", outPath, len(entries))
}
