package network

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestFileServeSlotBounded 验证 5.4 的全局文件服务限流：无论多少 goroutine 并发抢槽，
// 同时持有槽的数量都不超过 maxConcurrentFileServes（即"哈希+传输"的全局并发上限），
// 与连接数（256）解耦。
func TestFileServeSlotBounded(t *testing.T) {
	if maxConcurrentFileServes < 4 || maxConcurrentFileServes > 16 {
		t.Fatalf("maxConcurrentFileServes 应夹在 [4,16]，实际 %d", maxConcurrentFileServes)
	}
	if cap(fileServeSlots) != maxConcurrentFileServes {
		t.Fatalf("信号量容量 %d 应等于上限 %d", cap(fileServeSlots), maxConcurrentFileServes)
	}

	const workers = 64 // 远超上限，制造争抢
	var (
		inFlight atomic.Int32
		peak     atomic.Int32
		wg       sync.WaitGroup
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := acquireFileServeSlot()
			defer release()

			cur := inFlight.Add(1)
			for { // 记录峰值并发
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond) // 持槽片刻，逼并发叠加
			inFlight.Add(-1)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > int32(maxConcurrentFileServes) {
		t.Errorf("峰值并发 %d 超过全局上限 %d —— 限流失效", got, maxConcurrentFileServes)
	}
	// 全部释放后槽应清空
	if len(fileServeSlots) != 0 {
		t.Errorf("所有服务结束后信号量应清空，残留 %d", len(fileServeSlots))
	}
}
