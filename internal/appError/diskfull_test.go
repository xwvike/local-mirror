//go:build !windows

package appError

import (
	"fmt"
	"os"
	"syscall"
	"testing"
)

func TestIsDiskFull(t *testing.T) {
	// 模拟真实写盘失败的错误链：*os.PathError 包裹 ENOSPC，外层再有 fmt 包装
	enospc := &os.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC}
	if !IsDiskFull(enospc) {
		t.Error("ENOSPC 应判定为磁盘满")
	}
	if !IsDiskFull(fmt.Errorf("outer: %w", enospc)) {
		t.Error("包装后的 ENOSPC 应判定为磁盘满")
	}
	if !IsDiskFull(&os.PathError{Op: "write", Path: "/x", Err: syscall.EDQUOT}) {
		t.Error("EDQUOT（配额耗尽）应判定为磁盘满")
	}
	if IsDiskFull(&os.PathError{Op: "open", Path: "/x", Err: syscall.EACCES}) {
		t.Error("权限错误不应判定为磁盘满")
	}
	if IsDiskFull(nil) {
		t.Error("nil 不应判定为磁盘满")
	}
}
