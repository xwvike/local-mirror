package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"local-mirror/config"
	"local-mirror/internal/tree"
)

// newTestWatch 造一个带指定描述符预算的 ScoreWatch，同步根为临时目录
func newTestWatch(t *testing.T, budget int) (*ScoreWatch, string) {
	t.Helper()
	root := t.TempDir()
	config.StartPath = root
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	sw := &ScoreWatch{
		Watcher: w, tier1Limit: budget, tier1Budget: budget, tier1Scale: 1,
		heatMap: make(map[string]*HeatScore),
	}
	sw.fdCeiling, _ = fdLimit()
	return sw, root
}

func makeDir(t *testing.T, root, name string, files int) {
	t.Helper()
	d := filepath.Join(root, name)
	if err := os.MkdirAll(d, 0755); err != nil {
		t.Fatal(err)
	}
	for j := range files {
		if err := os.WriteFile(filepath.Join(d, fmt.Sprintf("f%d", j)), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestWatchCostOfMatchesKqueue 预算的计价前提：darwin 上 kqueue 为目录及其
// 每个条目各占一个描述符。整套预算都建立在这个等式上，它要是变了，预算
// 就会系统性地算错——所以用真实的 Add 盯住它
func TestWatchCostOfMatchesKqueue(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("kqueue 的按条目计价只适用于 darwin")
	}
	const files = 40
	sw, root := newTestWatch(t, 1<<20)
	makeDir(t, root, "d", files)

	before, _ := openFDCount()
	if err := sw.Watcher.Add(filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}
	after, _ := openFDCount()

	if got, want := after-before, watchCostOf(files); got != want {
		t.Errorf("一个 %d 条目的目录实占 %d 个描述符，watchCostOf 记 %d——预算的计价前提已失效",
			files, got, want)
	}
}

// TestAddHeatRespectsBudget 事件路径必须守预算。
// 回归：addHeat 过去完全不看预算，批量建目录（pnpm install 之类）能让 watch
// 无上限增长到把进程描述符耗尽——实测 200 个目录就冲到 10200 个描述符、
// 超预算 102 倍，连 accept 都拿不到描述符，整个地址族随之停服
func TestAddHeatRespectsBudget(t *testing.T) {
	const newDirs, filesPerDir, budget = 200, 50, 100
	sw, root := newTestWatch(t, budget)

	before, _ := openFDCount()
	for i := range newDirs {
		p := fmt.Sprintf("pkg%04d", i)
		makeDir(t, root, p, filesPerDir)
		sw.addHeat(p, &tree.Node{Path: p, IsDir: true, ModTime: time.Now()})
	}
	after, _ := openFDCount()

	if grew := after - before; grew > budget {
		t.Errorf("事件路径建了 %d 个目录后描述符增长 %d，超出预算 %d", newDirs, grew, budget)
	}
	if sw.usedWatches > budget {
		t.Errorf("账本 usedWatches=%d 超出预算 %d", sw.usedWatches, budget)
	}
	// 超预算的目录必须退到 tier2 轮询，而不是凭空消失——否则它们的变更
	// 永远不进源端树，汇端的全量扫描也救不回来（COR-04）
	if len(sw.tier1)+len(sw.tier2) != newDirs {
		t.Errorf("tier1(%d) + tier2(%d) != %d，有目录从两级监控里丢了",
			len(sw.tier1), len(sw.tier2), newDirs)
	}
	if len(sw.tier2) == 0 {
		t.Error("预算远小于需求，却没有任何目录降级到 tier2")
	}
	t.Logf("描述符增长 %d（预算 %d）；tier1 %d 个、tier2 %d 个",
		after-before, budget, len(sw.tier1), len(sw.tier2))
}

// TestRemoveHeatReturnsBudget 目录消失后必须归还预算，否则账本只增不减，
// 实时监控会被饿死在"预算已满"上
func TestRemoveHeatReturnsBudget(t *testing.T) {
	sw, root := newTestWatch(t, 1<<20)
	makeDir(t, root, "d", 10)
	sw.addHeat("d", &tree.Node{Path: "d", IsDir: true, ModTime: time.Now()})

	if sw.usedWatches == 0 {
		t.Fatal("addHeat 成功后账本应记上开销")
	}
	if len(sw.tier1) != 1 {
		t.Fatalf("该目录应进 tier1，实际 tier1=%d tier2=%d", len(sw.tier1), len(sw.tier2))
	}
	spent := sw.usedWatches

	sw.removeHeat("d")
	if sw.usedWatches != 0 {
		t.Errorf("removeHeat 后账本应归零（曾花 %d），实际仍为 %d", spent, sw.usedWatches)
	}
}

// TestAdmitWatchStopsAtFDWaterMark 账本还有余量，但实测描述符水位已高时
// 也必须拒绝——fsnotify 会为已监视目录里新建的每个文件自动再开描述符，
// 这条路不经过本模块的 Add，账本看不见，只有实测水位是真相
func TestAdmitWatchStopsAtFDWaterMark(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不按描述符计价")
	}
	sw, _ := newTestWatch(t, 1<<20) // 账本给足
	if !sw.admitWatch(1) {
		t.Fatal("水位正常时应放行")
	}
	// 把额度压到当前占用之下，水位必然越过高水位线
	open, ok := sw.fdProbe.level()
	if !ok {
		t.Skip("描述符探针不可用")
	}
	sw.fdCeiling = open
	sw.fdProbe = fdWaterMark{} // 清掉缓存，强制重新探测
	if sw.admitWatch(1) {
		t.Error("实测水位已越过高水位线，仍然放行了")
	}
}
