package network

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"testing"
)

// TestDecodeFileDataBounds 声称超大 DataLength 的小消息必须被拒，不触发巨量分配
func TestDecodeFileDataBounds(t *testing.T) {
	buf := new(bytes.Buffer)
	buf.Write(make([]byte, 16))                                 // SessionID
	_ = binary.Write(buf, binary.BigEndian, uint64(0))          // Offset
	_ = binary.Write(buf, binary.BigEndian, uint32(0xFFFFFFFF)) // DataLength 谎报 4GB
	// 实际后面没有数据
	if _, err := decodeFileData(buf.Bytes()); err == nil {
		t.Error("超大 DataLength 未被拒绝")
	}
}

// TestDecodeTreeResponseBounds 同上，针对树响应
func TestDecodeTreeResponseBounds(t *testing.T) {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.BigEndian, uint16(1)) // pathLen
	buf.WriteByte('.')                                 // path
	_ = binary.Write(buf, binary.BigEndian, uint32(0xFFFFFFFF))
	if _, err := decodeTreeResponse(buf.Bytes()); err == nil {
		t.Error("超大 tree DataLength 未被拒绝")
	}
}

// TestDecodeRecentChangeResponseBounds 谎报 changeCount 必须被拒（否则 make([]string) OOM）
func TestDecodeRecentChangeResponseBounds(t *testing.T) {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.BigEndian, uint32(0))          // ServerID
	_ = binary.Write(buf, binary.BigEndian, int64(0))           // CoveredUntil
	_ = binary.Write(buf, binary.BigEndian, uint32(0xFFFFFFFF)) // changeCount 谎报 40 亿
	if _, err := decodeRecentChangeResponse(buf.Bytes()); err == nil {
		t.Error("超大 changeCount 未被拒绝")
	}
}

// TestDecodeRoundTripStillWorks 合法消息仍能正常往返（不误伤）。
// 路径按线格式约定（protocol.go）：解码结果是本机分隔符形式
func TestDecodeRoundTripStillWorks(t *testing.T) {
	orig := RecentChangeResponseMessage{
		ServerID:     42,
		CoveredUntil: 12345,
		Changes:      []string{"a", "sub/b", "深/目录"},
	}
	got, err := decodeRecentChangeResponse(encodeRecentChangeResponse(orig))
	if err != nil {
		t.Fatalf("合法消息被误拒: %v", err)
	}
	if len(got.Changes) != 3 || got.Changes[2] != filepath.FromSlash("深/目录") || got.ServerID != 42 {
		t.Errorf("往返结果不符: %+v", got)
	}

	fd := FileDataMessage{Offset: 5, DataLength: 4, Data: []byte("data")}
	gotFD, err := decodeFileData(encodeFileData(fd))
	if err != nil || string(gotFD.Data) != "data" {
		t.Errorf("FileData 往返失败: %v %q", err, gotFD.Data)
	}
}
