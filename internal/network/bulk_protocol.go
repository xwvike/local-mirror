package network

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// BulkRequestMessage 首次全量推送的规划 / 开始请求（两者同一格式）
type BulkRequestMessage struct {
	Ignore       []string // 汇端忽略规则，源端不推送命中项
	Cursor       string   // 续推游标：跳过遍历顺序中不晚于它的条目；空 = 从头推送
	ResumePath   string   // 上次中断时正在接收的文件，源端推到它时从 ResumeOffset 续传
	ResumeOffset uint64
}

// BulkPlanResponseMessage 本次将推送的文件数与字节数（汇端据此预检磁盘空间）
type BulkPlanResponseMessage struct {
	Files uint64
	Bytes uint64
}

const (
	BulkEntryDir  uint8 = 1
	BulkEntryFile uint8 = 2
)

// BulkEntryMessage 推送流中的一个条目。文件条目的 SessionID/FileHash/Size/Offset
// 与 FileResponse 同义，其后紧跟同一会话的 FileData 与 FileComplete
type BulkEntryMessage struct {
	Kind      uint8
	Path      string
	Mode      uint32
	ModTime   int64 // Unix 纳秒
	Size      uint64
	Offset    uint64
	SessionID [16]byte
	FileHash  [32]byte
}

// BulkEndMessage 推送流结束，携带实际推送的文件数与字节数（续传的文件只计本次发送的部分）
type BulkEndMessage struct {
	Files uint64
	Bytes uint64
}

func writeString(buf *bytes.Buffer, s string) {
	_ = binary.Write(buf, binary.BigEndian, uint16(len(s)))
	buf.WriteString(s)
}

func readString(buf *bytes.Reader) (string, error) {
	var n uint16
	if err := binary.Read(buf, binary.BigEndian, &n); err != nil {
		return "", err
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(buf, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func encodeBulkRequest(msg BulkRequestMessage) []byte {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.BigEndian, uint16(len(msg.Ignore)))
	for _, p := range msg.Ignore {
		writeString(buf, p)
	}
	writeWirePath(buf, msg.Cursor)
	writeWirePath(buf, msg.ResumePath)
	_ = binary.Write(buf, binary.BigEndian, msg.ResumeOffset)
	return buf.Bytes()
}

func decodeBulkRequest(data []byte) (BulkRequestMessage, error) {
	var msg BulkRequestMessage
	buf := bytes.NewReader(data)
	var n uint16
	if err := binary.Read(buf, binary.BigEndian, &n); err != nil {
		return msg, err
	}
	// 每条规则至少 2 字节长度前缀，超过剩余字节一半的计数必然是伪造的
	if int(n) > buf.Len()/2 {
		return msg, fmt.Errorf("ignore pattern count %d exceeds plausible max for %d remaining bytes", n, buf.Len())
	}
	for range n {
		p, err := readString(buf)
		if err != nil {
			return msg, err
		}
		msg.Ignore = append(msg.Ignore, p)
	}
	var err error
	if msg.Cursor, err = readWirePath(buf); err != nil {
		return msg, err
	}
	if msg.ResumePath, err = readWirePath(buf); err != nil {
		return msg, err
	}
	err = binary.Read(buf, binary.BigEndian, &msg.ResumeOffset)
	return msg, err
}

func encodeBulkPlanResponse(msg BulkPlanResponseMessage) []byte {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.BigEndian, msg.Files)
	_ = binary.Write(buf, binary.BigEndian, msg.Bytes)
	return buf.Bytes()
}

func decodeBulkPlanResponse(data []byte) (BulkPlanResponseMessage, error) {
	var msg BulkPlanResponseMessage
	buf := bytes.NewReader(data)
	if err := binary.Read(buf, binary.BigEndian, &msg.Files); err != nil {
		return msg, err
	}
	err := binary.Read(buf, binary.BigEndian, &msg.Bytes)
	return msg, err
}

func encodeBulkEntry(msg BulkEntryMessage) []byte {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.BigEndian, msg.Kind)
	writeWirePath(buf, msg.Path)
	_ = binary.Write(buf, binary.BigEndian, msg.Mode)
	_ = binary.Write(buf, binary.BigEndian, msg.ModTime)
	_ = binary.Write(buf, binary.BigEndian, msg.Size)
	_ = binary.Write(buf, binary.BigEndian, msg.Offset)
	buf.Write(msg.SessionID[:])
	buf.Write(msg.FileHash[:])
	return buf.Bytes()
}

func decodeBulkEntry(data []byte) (BulkEntryMessage, error) {
	var msg BulkEntryMessage
	buf := bytes.NewReader(data)
	if err := binary.Read(buf, binary.BigEndian, &msg.Kind); err != nil {
		return msg, err
	}
	var err error
	if msg.Path, err = readWirePath(buf); err != nil {
		return msg, err
	}
	for _, v := range []any{&msg.Mode, &msg.ModTime, &msg.Size, &msg.Offset} {
		if err := binary.Read(buf, binary.BigEndian, v); err != nil {
			return msg, err
		}
	}
	if _, err := io.ReadFull(buf, msg.SessionID[:]); err != nil {
		return msg, err
	}
	_, err = io.ReadFull(buf, msg.FileHash[:])
	return msg, err
}

func encodeBulkEnd(msg BulkEndMessage) []byte {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.BigEndian, msg.Files)
	_ = binary.Write(buf, binary.BigEndian, msg.Bytes)
	return buf.Bytes()
}

func decodeBulkEnd(data []byte) (BulkEndMessage, error) {
	var msg BulkEndMessage
	buf := bytes.NewReader(data)
	if err := binary.Read(buf, binary.BigEndian, &msg.Files); err != nil {
		return msg, err
	}
	err := binary.Read(buf, binary.BigEndian, &msg.Bytes)
	return msg, err
}
