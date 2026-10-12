package watcher

import (
	"runtime"
	"testing"
)

// TestWatchBudgetLeavesHeadroom 预算必须从本进程的描述符额度推算，
// 并给进程其余用途留出余量。回归：旧实现拿 sysctl 的系统天花板当预算，
// 在默认 macOS（kern.maxfilesperproc=10240）上算出 tier1 名额 5120，
// 等于允许监视器独占全部额度的一半，fd 耗尽后连 accept 都拿不到描述符
func TestWatchBudgetLeavesHeadroom(t *testing.T) {
	const macDefault = 10240 // 默认 macOS 的 kern.maxfilesperproc
	budget := watchBudget(macDefault, true)
	tier1 := budget / 2 // InitWatcher 的换算

	if tier1 >= macDefault/2 {
		t.Errorf("tier1 quota %d is at least half of the %d fd allowance: no headroom for connections, the tree DB, logs and file serving",
			tier1, macDefault)
	}
	// 预算 + 预留 + 漂移（实测可达预估的 2 倍）仍须留在额度内
	if worst := tier1*2 + fdReserve; worst >= macDefault {
		t.Errorf("tier1 quota %d with 2x kqueue drift plus the %d reserve reaches %d, over the %d allowance",
			tier1, fdReserve, worst, macDefault)
	}
}

func TestWatchBudgetEdges(t *testing.T) {
	if got := watchBudget(0, false); got != minWatchBudget {
		t.Errorf("probe unavailable: got %d, want the conservative floor %d", got, minWatchBudget)
	}
	// 额度小于预留时不能算出负数或 0
	if got := watchBudget(512, true); got != minWatchBudget {
		t.Errorf("tiny allowance: got %d, want %d", got, minWatchBudget)
	}
	// 额度极大时压在保守上限
	if got := watchBudget(1<<30, true); got != maxWatchesCap {
		t.Errorf("huge allowance: got %d, want the cap %d", got, maxWatchesCap)
	}
}

// TestFDProbes 描述符探针在本平台上可用（Windows 上按设计不可用）
func TestFDProbes(t *testing.T) {
	limit, ok := fdLimit()
	if runtime.GOOS == "windows" {
		if ok {
			t.Error("fdLimit should report unavailable on Windows")
		}
		return
	}
	if !ok || limit <= 0 {
		t.Fatalf("fdLimit() = %d, %v; want a positive limit", limit, ok)
	}
	open, ok := openFDCount()
	if !ok || open <= 0 {
		t.Fatalf("openFDCount() = %d, %v; want a positive count", open, ok)
	}
	if open > limit {
		t.Errorf("open fds %d exceeds the limit %d", open, limit)
	}
	t.Logf("this process: %d of %d descriptors open", open, limit)
}

// TestEffectiveTier1LimitHysteresis 描述符吃紧时名额收缩、回落后放开。
// 用 fdCeiling 造水位：置 1 则真实占用必然远超高水位，置极大则必然低于
// 低水位——不必注入假的 openFDCount
func TestEffectiveTier1LimitHysteresis(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("descriptor water marks do not apply on Windows")
	}
	sw := &ScoreWatch{tier1Limit: 4096, fdCeiling: 1, tier1Scale: 1}

	if got := sw.effectiveTier1Limit(); got != 2048 {
		t.Fatalf("first round over high water: got %d, want 2048 (halved)", got)
	}
	if got := sw.effectiveTier1Limit(); got != 1024 {
		t.Fatalf("second round still over high water: got %d, want 1024", got)
	}
	// 持续吃紧也不能收缩到 0：最热的几个目录必须保留实时监控
	for range 20 {
		sw.effectiveTier1Limit()
	}
	if got := sw.effectiveTier1Limit(); got < minTier1Limit {
		t.Errorf("sustained pressure shrank the quota to %d, below the floor %d", got, minTier1Limit)
	}
	if sw.tier1Scale > tier1ScaleMax {
		t.Errorf("tier1Scale %d ran past the cap %d", sw.tier1Scale, tier1ScaleMax)
	}

	// 水位回落：名额逐步放开，最终回到上限
	sw.fdCeiling = 1 << 30
	for range 20 {
		sw.effectiveTier1Limit()
	}
	if got := sw.effectiveTier1Limit(); got != 4096 {
		t.Errorf("after recovery: got %d, want the full quota 4096", got)
	}
	if sw.tier1Scale != 1 {
		t.Errorf("tier1Scale = %d, want 1 after recovery", sw.tier1Scale)
	}
}

// TestEffectiveTier1LimitNoProbe 探针不可用时保持上限不变（Windows 路径）
func TestEffectiveTier1LimitNoProbe(t *testing.T) {
	sw := &ScoreWatch{tier1Limit: 4096, fdCeiling: 0, tier1Scale: 1}
	if got := sw.effectiveTier1Limit(); got != 4096 {
		t.Errorf("got %d, want the quota untouched at 4096", got)
	}
}
