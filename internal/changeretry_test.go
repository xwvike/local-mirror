package app

import (
	"reflect"
	"testing"
)

// TestChangeRetryBookkeeping 变更追踪里没处理成功的目录要在下一轮重试，
// 成功即消失、连续失败超过上限就放手给全量扫描，全量扫描作废全部登记。
// 修复前：失败目录不登记，游标照样推过它的变更，最长要等一个 cooldown（默认 30 分钟）
func TestChangeRetryBookkeeping(t *testing.T) {
	resetRetries()
	t.Cleanup(resetRetries)

	// 第 1 轮：两个目录失败，同一目录重复登记只算一次
	if due := beginRetryRound(); len(due) != 0 {
		t.Fatalf("初始不应有待重试目录，得 %v", due)
	}
	noteRetry("b")
	noteRetry("a")
	noteRetry("a")

	// 第 2 轮：两者都被取出（按路径排序）；a 再次失败，b 成功（不再登记）
	if due := beginRetryRound(); !reflect.DeepEqual(due, []string{"a", "b"}) {
		t.Fatalf("第 2 轮应重试 [a b]，得 %v", due)
	}
	noteRetry("a")

	// 第 3 轮：只剩 a；b 成功后不再出现
	if due := beginRetryRound(); !reflect.DeepEqual(due, []string{"a"}) {
		t.Fatalf("第 3 轮应只重试 [a]，得 %v", due)
	}
	noteRetry("a") // a 第 3 次失败，仍在上限内

	if due := beginRetryRound(); !reflect.DeepEqual(due, []string{"a"}) {
		t.Fatalf("第 4 轮应仍重试 [a]，得 %v", due)
	}
	noteRetry("a") // 第 4 次失败：超过 maxChangeRetries，放手给全量扫描

	if due := beginRetryRound(); len(due) != 0 {
		t.Fatalf("连续失败超过 %d 次后应放手，得 %v", maxChangeRetries, due)
	}
}

// TestFullScanResetsRetries 全量扫描覆盖整棵树，之前的重试登记必须作废，
// 扫描中再失败的目录从 1 次重新计数
func TestFullScanResetsRetries(t *testing.T) {
	resetRetries()
	t.Cleanup(resetRetries)

	beginRetryRound()
	noteRetry("x")
	beginRetryRound()
	noteRetry("x") // x 已失败 2 次

	resetRetries() // 全量扫描开始
	noteRetry("x") // 扫描中再次失败
	if got := retryDirs["x"]; got != 1 {
		t.Errorf("全量扫描后失败计数应从 1 重新开始，得 %d", got)
	}
	if due := beginRetryRound(); !reflect.DeepEqual(due, []string{"x"}) {
		t.Errorf("全量扫描中失败的目录应在下一轮重试，得 %v", due)
	}
}
