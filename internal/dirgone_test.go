package app

import (
	"errors"
	"fmt"
	"testing"

	"local-mirror/internal/appError"
	"local-mirror/internal/network"
)

// TestDirGoneOnlyMatchesNotFound 只有服务端明确回 ErrCodeNotFound 才算"目录已不存在"。
// 其余错误必须落回常规处理：连接错误要触发重连重试，权限/越界要照常报出来，
// 否则会被当成正常删除静默跳过，故障就此隐形
func TestDirGoneOnlyMatchesNotFound(t *testing.T) {
	const path = "some/dir"

	cases := []struct {
		name string
		err  error
		gone bool
	}{
		{"目录不存在", &network.RealityError{Code: network.ErrCodeNotFound, Path: path, Message: "error getting tree contents"}, true},
		{"包装后的目录不存在", fmt.Errorf("wrapped: %w", &network.RealityError{Code: network.ErrCodeNotFound, Path: path}), true},
		{"权限拒绝", &network.RealityError{Code: network.ErrCodePermissionDenied, Path: path}, false},
		{"路径越界", &network.RealityError{Code: network.ErrCodeOutOfRoot, Path: path}, false},
		{"未归类错误", &network.RealityError{Code: network.ErrCodeInternal, Path: path}, false},
		{"连接错误", fmt.Errorf("%w: read timeout", appError.ErrConnection), false},
		{"普通错误", errors.New("boom"), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dirGone(c.err, path)
			if c.gone {
				if got == nil {
					t.Fatal("应判定为目录已不存在")
				}
				if !errors.Is(got, errDirGone) {
					t.Fatalf("结果应可被 errors.Is(errDirGone) 命中, got: %v", got)
				}
				return
			}
			if got != nil {
				t.Fatalf("不该判定为目录已不存在, got: %v", got)
			}
		})
	}
}
