package network

import (
	"path/filepath"
	"strings"
	"testing"
)

// 线格式路径约定（见 protocol.go）：编码后的字节流里路径一律以 "/" 分隔，
// 解码后回到本机分隔符。Unix 上两个方向都是恒等变换，这些测试在两类平台
// 都必须通过；Windows 上还额外验证字节流里不残留反斜杠。

// wireHasNativeSeparator 检查编码结果中是否泄漏了非 "/" 的本机分隔符。
// 只在本机分隔符不是 "/" 的平台（Windows）有意义，Unix 上恒为 false
func wireHasNativeSeparator(raw []byte) bool {
	return filepath.Separator != '/' &&
		strings.ContainsRune(string(raw), filepath.Separator)
}

func TestFileRequestWirePath(t *testing.T) {
	native := filepath.Join("deep", "sub", "f.txt")
	msg := FileRequestMessage{FilePath: native, Offset: 42}
	raw := encodeFileRequest(msg)
	if wireHasNativeSeparator(raw) {
		t.Errorf("编码结果泄漏了本机分隔符: %q", raw)
	}
	if !strings.Contains(string(raw), "deep/sub/f.txt") {
		t.Errorf("编码结果里应有 / 分隔的路径: %q", raw)
	}

	decoded, err := decodeFileRequest(raw)
	if err != nil {
		t.Fatalf("decodeFileRequest: %v", err)
	}
	if decoded.FilePath != native {
		t.Errorf("解码路径 = %q, 应为本机形式 %q", decoded.FilePath, native)
	}
	if decoded.Offset != 42 {
		t.Errorf("Offset = %d, 应为 42", decoded.Offset)
	}
}

func TestTreeRequestWirePath(t *testing.T) {
	native := filepath.Join("wnested", "deep")
	cursor := filepath.Join("wnested", "deep", "m.txt")
	msg := TreeRequestMessage{RootPath: native, ContinueFrom: cursor}
	raw := encodeTreeRequest(msg)
	if wireHasNativeSeparator(raw) {
		t.Errorf("编码结果泄漏了本机分隔符: %q", raw)
	}

	decoded, err := decodeTreeRequest(raw)
	if err != nil {
		t.Fatalf("decodeTreeRequest: %v", err)
	}
	if decoded.RootPath != native {
		t.Errorf("解码路径 = %q, 应为本机形式 %q", decoded.RootPath, native)
	}
	if decoded.ContinueFrom != cursor {
		t.Errorf("解码游标 = %q, 应为本机形式 %q", decoded.ContinueFrom, cursor)
	}
}

func TestTreeResponseWirePath(t *testing.T) {
	cursor := filepath.Join("a", "b")
	payload := []byte(`[{"path":"x"}]`) // Data 是不透明负载，路径转换不碰它
	msg := TreeResponseMessage{
		ContinueFrom: cursor,
		DataLength:   uint32(len(payload)),
		Data:         payload,
	}
	raw := encodeTreeResponse(msg)
	if wireHasNativeSeparator(raw) {
		t.Errorf("编码结果泄漏了本机分隔符: %q", raw)
	}
	decoded, err := decodeTreeResponse(raw)
	if err != nil {
		t.Fatalf("decodeTreeResponse: %v", err)
	}
	if decoded.ContinueFrom != cursor {
		t.Errorf("解码游标 = %q, 应为本机形式 %q", decoded.ContinueFrom, cursor)
	}
	if string(decoded.Data) != string(payload) {
		t.Errorf("Data 负载被改动: %q", decoded.Data)
	}
}

func TestRecentChangeResponseWirePath(t *testing.T) {
	changes := []string{".", "deep", filepath.Join("deep", "sub")}
	msg := RecentChangeResponseMessage{
		Changes:      changes,
		ServerID:     7,
		CoveredUntil: 99,
	}
	raw := encodeRecentChangeResponse(msg)
	if wireHasNativeSeparator(raw) {
		t.Errorf("编码结果泄漏了本机分隔符: %q", raw)
	}

	decoded, err := decodeRecentChangeResponse(raw)
	if err != nil {
		t.Fatalf("decodeRecentChangeResponse: %v", err)
	}
	if len(decoded.Changes) != len(changes) {
		t.Fatalf("Changes 数量 = %d, 应为 %d", len(decoded.Changes), len(changes))
	}
	for i, want := range changes {
		if decoded.Changes[i] != want {
			t.Errorf("Changes[%d] = %q, 应为本机形式 %q", i, decoded.Changes[i], want)
		}
	}
	if decoded.FullResync {
		t.Error("FullResync 应为 false")
	}
}

// TestWirePathFromUnixPeer 模拟收到 Unix 对端（线格式即 "/"）的路径：
// 解码结果必须是本机分隔符形式，Windows 上即完成 / → \ 的转换
func TestWirePathFromUnixPeer(t *testing.T) {
	slash := "deep/sub/f.txt"
	msg := FileRequestMessage{FilePath: slash}
	decoded, err := decodeFileRequest(encodeFileRequest(msg))
	if err != nil {
		t.Fatalf("decodeFileRequest: %v", err)
	}
	if want := filepath.FromSlash(slash); decoded.FilePath != want {
		t.Errorf("解码路径 = %q, 应为本机形式 %q", decoded.FilePath, want)
	}
}

// TestErrorMessageWirePath 结构化错误的 Path 字段同样走线格式路径约定
func TestErrorMessageWirePath(t *testing.T) {
	native := filepath.Join("deep", "sub", "f.txt")
	msg := ErrorMessage{Code: ErrCodePermissionDenied, Path: native, Message: "permission denied"}
	raw := encodeErrorMessage(msg)
	if wireHasNativeSeparator(raw) {
		t.Errorf("编码结果泄漏了本机分隔符: %q", raw)
	}
	decoded, err := decodeErrorMessage(raw)
	if err != nil {
		t.Fatalf("decodeErrorMessage: %v", err)
	}
	if decoded.Code != ErrCodePermissionDenied || decoded.Path != native || decoded.Message != "permission denied" {
		t.Errorf("往返结果不符: %+v", decoded)
	}
}
