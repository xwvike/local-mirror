package app

import (
	"errors"
	"fmt"
	"testing"

	"local-mirror/internal/appError"
	"local-mirror/internal/network"
)

// TestExecuteTaskWithClientErrorPropagation 验证 §5.1：executeTaskWithClient 不再把非连接类
// 任务错误吞成 nil（那会让 DB/初始扫描等真实失败在上层被当成成功），而是如实上抛；
// 连接类错误照常包成 deprecated 上抛；成功则返回 nil。
func TestExecuteTaskWithClientErrorPropagation(t *testing.T) {
	fc := &network.FileClient{} // State 零值 = Waiting，非 Deprecated

	// 成功 → nil
	if err := executeTaskWithClient("ok", fc, func(*network.FileClient) error { return nil }); err != nil {
		t.Errorf("taskFunc 成功时应返回 nil，得 %v", err)
	}

	// 非连接类错误 → 上抛（不再吞成 nil）
	sentinel := errors.New("db exploded")
	err := executeTaskWithClient("db-err", fc, func(*network.FileClient) error { return sentinel })
	if err == nil {
		t.Fatal("非连接类任务错误必须上抛，不能被吞成 nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("上抛的错误应包含原错误，得 %v", err)
	}

	// 连接类错误 → 上抛并保留 ErrConnection
	connErr := fmt.Errorf("%w: dropped", appError.ErrConnection)
	err = executeTaskWithClient("conn-err", fc, func(*network.FileClient) error { return connErr })
	if err == nil || !errors.Is(err, appError.ErrConnection) {
		t.Errorf("连接类错误应上抛并保留 ErrConnection，得 %v", err)
	}
}
