package network

import (
	"fmt"
	"local-mirror/internal/tree"
	"testing"
)

// TestNegotiateVersion 覆盖协商矩阵：相等、区间重叠、区间不相交
func TestNegotiateVersion(t *testing.T) {
	cases := []struct {
		name                             string
		localVer, localMin               uint16
		peerVer, peerMin                 uint16
		wantAgreed                       uint16
		wantOK                           bool
	}{
		{"两端相等", 3, 3, 3, 3, 3, true},
		{"对端更新且向下兼容", 3, 3, 4, 3, 3, true},
		{"本端更新且向下兼容", 4, 3, 3, 3, 3, true},
		{"取交集最高值", 5, 3, 4, 2, 4, true},
		{"对端太旧", 3, 3, 2, 2, 0, false},
		{"本端太旧", 2, 2, 3, 3, 0, false},
		{"区间完全错开", 5, 5, 3, 3, 0, false},
	}
	for _, c := range cases {
		agreed, ok := negotiateVersion(c.localVer, c.localMin, c.peerVer, c.peerMin)
		if ok != c.wantOK {
			t.Errorf("%s: ok=%v, 应为 %v", c.name, ok, c.wantOK)
			continue
		}
		if ok && agreed != c.wantAgreed {
			t.Errorf("%s: agreed=%d, 应为 %d", c.name, agreed, c.wantAgreed)
		}
	}
}

// TestHandshakeRoundTrip v3 握手消息完整往返
func TestHandshakeRoundTrip(t *testing.T) {
	orig := HandshakeMessage{
		Version:     3,
		MinVersion:  3,
		UUID:        0xDEADBEEF,
		Role:        1,
		FeatureBits: 0,
	}
	got, err := decodeHandshake(encodeHandshake(orig))
	if err != nil {
		t.Fatalf("decodeHandshake: %v", err)
	}
	if got != orig {
		t.Errorf("往返结果不符: %+v != %+v", got, orig)
	}
}

// TestHandshakeRejectsShortBody v2 及更早的握手体（7 字节）必须解码失败，
// 服务端据此走"版本不符"拒绝路径
func TestHandshakeRejectsShortBody(t *testing.T) {
	v2body := []byte{0x00, 0x02, 0xAA, 0xBB, 0xCC, 0xDD, 0x01} // Version=2|UUID|Role
	if _, err := decodeHandshake(v2body); err == nil {
		t.Error("7 字节的 v2 握手体不应能按 v3 布局解码成功")
	}
}

// TestTrailingAppendTolerated 同版本演进机制（见 protocol.go 约定）：
// 消息体尾部追加的未知字段必须被现有解码器静默忽略
func TestTrailingAppendTolerated(t *testing.T) {
	future := append(encodeHandshake(HandshakeMessage{Version: 3, MinVersion: 3, UUID: 7, Role: 1}),
		0xFF, 0xEE, 0xDD) // 假想的未来新字段
	got, err := decodeHandshake(future)
	if err != nil {
		t.Fatalf("尾部追加字段导致解码失败（违反演进约定）: %v", err)
	}
	if got.UUID != 7 {
		t.Errorf("已知字段被尾部数据污染: %+v", got)
	}

	futureReq := append(encodeFileRequest(FileRequestMessage{FilePath: "a/b", Offset: 9}), 0x01, 0x02)
	gotReq, err := decodeFileRequest(futureReq)
	if err != nil {
		t.Fatalf("FileRequest 尾部追加导致解码失败: %v", err)
	}
	if gotReq.Offset != 9 {
		t.Errorf("已知字段被尾部数据污染: %+v", gotReq)
	}
}

// TestPageTreeEntries 树分页：排序、首页、续页、末页、空目录
func TestPageTreeEntries(t *testing.T) {
	entries := make([]tree.Node, 0, 25)
	for i := 24; i >= 0; i-- { // 逆序放入，验证分页前排序
		entries = append(entries, tree.Node{Path: fmt.Sprintf("f%02d", i)})
	}

	var got []string
	cursor := ""
	pages := 0
	for {
		page, next := pageTreeEntries(entries, cursor, 10)
		pages++
		for _, n := range page {
			got = append(got, n.Path)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if pages != 3 {
		t.Errorf("25 条 / 每页 10 应为 3 页，实际 %d 页", pages)
	}
	if len(got) != 25 {
		t.Fatalf("聚合条目数 = %d, 应为 25", len(got))
	}
	for i, p := range got {
		if want := fmt.Sprintf("f%02d", i); p != want {
			t.Errorf("第 %d 条 = %q, 应为 %q（排序或分页边界错误）", i, p, want)
		}
	}

	// 空目录
	if page, next := pageTreeEntries(nil, "", 10); len(page) != 0 || next != "" {
		t.Errorf("空目录应返回空页: page=%d next=%q", len(page), next)
	}
	// 恰好整页：不应产生多余的空页游标
	exact := make([]tree.Node, 10)
	for i := range exact {
		exact[i] = tree.Node{Path: fmt.Sprintf("g%02d", i)}
	}
	if _, next := pageTreeEntries(exact, "", 10); next != "" {
		t.Errorf("恰好一页时 next 应为空，实际 %q", next)
	}
}

// TestBuildRecentChangeResponse 变更响应的超限降级
func TestBuildRecentChangeResponse(t *testing.T) {
	small := []string{"a", "b", "a"} // 含重复
	resp := buildRecentChangeResponse(small, 100)
	if resp.FullResync {
		t.Error("小批量变更不应降级")
	}
	if len(resp.Changes) != 2 {
		t.Errorf("去重后应为 2 条，实际 %d", len(resp.Changes))
	}
	if resp.CoveredUntil != 100 {
		t.Errorf("CoveredUntil = %d, 应为 100", resp.CoveredUntil)
	}

	big := make([]string, changeFullResyncThreshold+1)
	for i := range big {
		big[i] = fmt.Sprintf("dir%06d", i)
	}
	resp = buildRecentChangeResponse(big, 200)
	if !resp.FullResync {
		t.Error("超过阈值应降级为 FullResync")
	}
	if len(resp.Changes) != 0 {
		t.Errorf("降级响应不应携带变更列表，实际 %d 条", len(resp.Changes))
	}
	if resp.CoveredUntil != 200 {
		t.Errorf("CoveredUntil = %d, 应为 200（游标必须照常推进）", resp.CoveredUntil)
	}

	// 降级响应的编解码往返
	got, err := decodeRecentChangeResponse(encodeRecentChangeResponse(resp))
	if err != nil {
		t.Fatalf("降级响应往返失败: %v", err)
	}
	if !got.FullResync || got.CoveredUntil != 200 {
		t.Errorf("降级响应往返结果不符: %+v", got)
	}
}
