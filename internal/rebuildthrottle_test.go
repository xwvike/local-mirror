package app

import (
	"testing"
	"time"

	"local-mirror/config"
)

// TestShouldRebuildLocalTree 验证 §5.3 的重建节流 + 漂移强制触发：
// 中继/源端从不在此重建；纯汇端距上次重建不足 localRebuildMinInterval 时跳过（标准节流），
// 距上次够久或 driftSuspected 置位时重建（补充触发无视节流），且 driftSuspected 读后清零。
func TestShouldRebuildLocalTree(t *testing.T) {
	saveMode := config.Mode
	saveLast := lastLocalRebuild
	defer func() {
		config.Mode = saveMode
		lastLocalRebuild = saveLast
		driftSuspected.Store(false)
	}()
	setMode := func(m string) { mm := m; config.Mode = &mm }

	// 中继/源端（ServesDownstream）：从不在此重建
	driftSuspected.Store(true) // 即便置了位
	setMode("reality")
	if shouldRebuildLocalTree() {
		t.Error("源端(reality)不该在此重建本地树")
	}
	setMode("relay")
	if shouldRebuildLocalTree() {
		t.Error("中继(relay)不该在此重建本地树")
	}
	driftSuspected.Store(false)

	// 纯汇端(mirror)
	setMode("mirror")

	// 刚重建过（节流窗口内）+ 无漂移 → 跳过
	lastLocalRebuild = time.Now()
	if shouldRebuildLocalTree() {
		t.Error("距上次重建不足节流窗口且无漂移，应跳过")
	}

	// 距上次够久 → 重建（标准节流放行）
	lastLocalRebuild = time.Now().Add(-2 * localRebuildMinInterval)
	if !shouldRebuildLocalTree() {
		t.Error("距上次重建超过节流窗口，应重建")
	}

	// 漂移置位 + 刚重建过 → 仍重建（补充触发无视节流），且读后清零
	lastLocalRebuild = time.Now()
	SuspectLocalDrift()
	if !shouldRebuildLocalTree() {
		t.Error("driftSuspected 置位应无视节流强制重建")
	}
	if driftSuspected.Load() {
		t.Error("driftSuspected 应在判定后被清零（Swap 语义）")
	}
	// 清零后、仍在节流窗口内 → 再次跳过（确认不是永久强制）
	lastLocalRebuild = time.Now()
	if shouldRebuildLocalTree() {
		t.Error("漂移标志清零后、节流窗口内应重新跳过")
	}
}
