package tree

import (
	"testing"
	"time"
)

// TestChangeLogSettledHoldsBackInflight 在途（键已定、尚未提交）的变更记录必须挡住
// 覆盖点：否则查询恰好落在定键与提交之间又跨了秒时，这条记录会被游标跨过去
func TestChangeLogSettledHoldsBackInflight(t *testing.T) {
	before := time.Now().Unix()
	if got := ChangeLogSettled(); got < before {
		t.Fatalf("no writes in flight: settled point should be the current second, got %d < %d", got, before)
	}

	stamp := before - 5
	changeInflightMu.Lock()
	changeInflight[stamp]++
	changeInflightMu.Unlock()
	t.Cleanup(func() {
		changeInflightMu.Lock()
		delete(changeInflight, stamp)
		changeInflightMu.Unlock()
	})

	if got := ChangeLogSettled(); got != stamp {
		t.Fatalf("a write in flight at %d must hold the settled point back, got %d", stamp, got)
	}
}
