package main

import (
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/status"
	"local-mirror/internal/watcher"
	"local-mirror/pkg/termstyle"
	"os"
	"time"

	"golang.org/x/term"
)

// heatMaxRows 终端里单个热度表最多展示的行数；其余（都是低分 tier2）折叠为计数。
// 观测关心的是"我干活的目录有没有拿到实时 watch"，高分在前已足够
const heatMaxRows = 40

// runHeatSingle 单实例 --heat：终端进实时刷新循环，管道则打印一次。
// heat.json 挂在同一个观测门上，投放心跳即触发源端刷新
func runHeatSingle(root string) {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		status.TouchObserve(root)
		liveLoop(func() { status.TouchObserve(root); renderHeatSingle(root) })
		status.ClearObserve(root)
	} else {
		since := time.Now()
		status.TouchObserve(root)
		status.AwaitFresh(root, since, 2*time.Second)
		renderHeatSingle(root)
		status.ClearObserve(root)
	}
}

// runHeatAll 全机 --heat --all：发现本机所有源实例并逐个展示热度表
func runHeatAll() {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		liveLoop(renderHeatAll)
	} else {
		for _, inst := range status.DiscoverInstances() {
			status.TouchObserve(inst.Root)
		}
		time.Sleep(observeWarm)
		renderHeatAll()
	}
}

// runHeatAggregate 多实例 --heat --config：聚合 YAML 每个任务的热度表
func runHeatAggregate(cfg *config.MultiConfig) {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		liveLoop(func() { renderHeatAggregate(cfg) })
	} else {
		for i := range cfg.Tasks {
			status.TouchObserve(cfg.Tasks[i].Path)
		}
		time.Sleep(observeWarm)
		renderHeatAggregate(cfg)
	}
}

// renderHeatSingle 渲染单个同步根的目录热度表（每帧重新读盘）
func renderHeatSingle(root string) {
	p := termstyle.NewPalette(os.Stdout)
	snap, err := watcher.LoadHeat(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: cannot read heat table: %v\n", err)
		return
	}
	fmt.Println()
	if snap == nil {
		fmt.Printf("  %sno heat table for%s %s\n", p.Dim, p.Reset, root)
		fmt.Printf("  %s(only a running source or relay builds one; a sink has none)%s\n", p.Dim, p.Reset)
		return
	}
	fmt.Printf("  %s%sheat%s   %s%s%s\n", p.Bold, p.Cyan, p.Reset, p.Dim, root, p.Reset)
	renderHeatTable(snap, p)
}

// renderHeatAll 从进程表发现本机所有实例，逐个展示各自的热度表（源才有）
func renderHeatAll() {
	p := termstyle.NewPalette(os.Stdout)
	instances := status.DiscoverInstances()
	fmt.Println()
	fmt.Printf("  %s%slocal-mirror heat%s   %s%d running on this host%s\n",
		p.Bold, p.Cyan, p.Reset, p.Dim, len(instances), p.Reset)
	shown := 0
	for _, inst := range instances {
		status.TouchObserve(inst.Root) // 请求各源下一帧刷新 heat.json
		snap, err := watcher.LoadHeat(inst.Root)
		if err != nil || snap == nil {
			continue // 汇实例无热度表，跳过
		}
		fmt.Printf("\n  %s%s%s\n", p.Bold, shortRoot(inst.Root), p.Reset)
		renderHeatTable(snap, p)
		shown++
	}
	if shown == 0 {
		fmt.Printf("\n  %sno source with a heat table found (sinks don't build one)%s\n", p.Dim, p.Reset)
	}
}

// renderHeatAggregate 逐个展示 YAML 每个任务的热度表（各读各自根下的 heat.json）
func renderHeatAggregate(cfg *config.MultiConfig) {
	p := termstyle.NewPalette(os.Stdout)
	fmt.Println()
	fmt.Printf("  %s%slocal-mirror heat%s   %s%d tasks%s\n", p.Bold, p.Cyan, p.Reset, p.Dim, len(cfg.Tasks), p.Reset)
	shown := 0
	for i := range cfg.Tasks {
		t := cfg.Tasks[i]
		status.TouchObserve(t.Path) // 请求各源下一帧刷新 heat.json
		snap, err := watcher.LoadHeat(t.Path)
		if err != nil || snap == nil {
			continue
		}
		fmt.Printf("\n  %s%s%s\n", p.Bold, t.Name, p.Reset)
		renderHeatTable(snap, p)
		shown++
	}
	if shown == 0 {
		fmt.Printf("\n  %sno task with a heat table found (only source/relay tasks build one)%s\n", p.Dim, p.Reset)
	}
}

// renderHeatTable 热度表主体：分数降序，tier1（实时 watch）绿标，超出 heatMaxRows
// 的低分尾部折叠为计数
func renderHeatTable(snap *watcher.HeatSnapshot, p termstyle.Palette) {
	stale := ""
	if snap.Stale() {
		stale = p.Yellow + "   (stale: source may have stopped)" + p.Reset
	}
	fmt.Printf("  %stier1 (real-time watch) %d/%d · tier2 (lazy poll) %d · %d dirs%s%s\n",
		p.Dim, snap.Tier1Count, snap.Tier1Limit, snap.Total-snap.Tier1Count, snap.Total, p.Reset, stale)
	if snap.Total == 0 {
		fmt.Printf("  %s(no directories scored yet)%s\n", p.Dim, p.Reset)
		return
	}
	fmt.Printf("  %s%s %s %s %s%s\n", p.Dim,
		padCell("SCORE", 9), padCell("TIER", 6), padCell("EVENTS", 8), "DIRECTORY", p.Reset)
	for i, e := range snap.Entries {
		if i >= heatMaxRows {
			fmt.Printf("  %s… +%d more (tier2, lower score)%s\n", p.Dim, len(snap.Entries)-heatMaxRows, p.Reset)
			break
		}
		tier, tcol := "tier2", p.Dim
		if e.Tier == 1 {
			tier, tcol = "tier1", p.Green
		}
		dir := e.Path
		if dir == "" || dir == "." {
			dir = ". (sync root)"
		}
		fmt.Printf("  %s %s%s%s %s %s\n",
			padCell(fmt.Sprintf("%.2f", e.Score), 9),
			tcol, padCell(tier, 6), p.Reset,
			padCell(fmt.Sprintf("%d", e.Events), 8), dir)
	}
}
