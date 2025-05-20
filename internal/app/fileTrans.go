package app

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/zeebo/blake3"
)

// 协议常量定义
const (
	// 魔术字
	MagicNumber uint32 = 0xF1E2D3C4 // 协议标识符

	// 消息类型
	MsgTypeHandshake    uint16 = 0x0001 // 握手请求/响应
	MsgTypeFileRequest  uint16 = 0x0002 // 文件传输请求
	MsgTypeFileResponse uint16 = 0x0003 // 文件传输响应
	MsgTypeFileData     uint16 = 0x0004 // 文件数据
	MsgTypeFileComplete uint16 = 0x0005 // 文件传输完成
	MsgTypeError        uint16 = 0x0006 // 错误消息
	MsgTypeAcknowledge  uint16 = 0x0007 // 确认消息

	// 状态码
	StatusOK            uint16 = 0x0000 // 正常
	StatusReject        uint16 = 0x0001 // 拒绝传输
	StatusFileNotFound  uint16 = 0x0002 // 文件不存在
	StatusInternalError uint16 = 0x0003 // 内部错误

	// 头部大小
	HeaderSize = 12
)

// 消息头定义
type MessageHeader struct {
	Magic        uint32 // 魔术字
	Type         uint16 // 消息类型
	BodyLength   uint32 // 消息体长度
	ReservedWord uint16 // 保留字段
}

// 握手消息
type HandshakeMessage struct {
	Version  uint16 // 协议版本
	ClientID uint32 // 客户端标识
}

// 文件请求消息
type FileRequestMessage struct {
	NameLength uint16 // 文件名长度
	Filename   string // 文件名
	Offset     uint64 // 起始偏移（断点续传用）
}

// 文件响应消息
type FileResponseMessage struct {
	Status    uint16   // 状态码
	SessionID uint32   // 会话ID
	FileSize  uint64   // 文件大小
	FileHash  [32]byte // 文件哈希值
}

// 文件数据消息
type FileDataMessage struct {
	SessionID  uint32 // 会话ID
	Offset     uint64 // 数据偏移
	DataLength uint32 // 数据长度
	Data       []byte // 数据内容
}

// 文件完成消息
type FileCompleteMessage struct {
	SessionID uint32   // 会话ID
	FileHash  [32]byte // 文件哈希值
}

// 错误消息
type ErrorMessage struct {
	ErrorCode    uint16 // 错误码
	MessageLen   uint16 // 消息长度
	ErrorMessage string // 错误消息
}

// 确认消息
type AcknowledgeMessage struct {
	SessionID uint32 // 会话ID
	Offset    uint64 // 确认偏移
	Status    uint16 // 确认状态
}

func encodeHeader(header MessageHeader) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, header.Magic)
	binary.Write(buf, binary.BigEndian, header.Type)
	binary.Write(buf, binary.BigEndian, header.BodyLength)
	binary.Write(buf, binary.BigEndian, header.ReservedWord)
	return buf.Bytes()
}

func decodeHeader(data []byte) (MessageHeader, error) {
	var header MessageHeader
	buf := bytes.NewReader(data)

	if err := binary.Read(buf, binary.BigEndian, &header.Magic); err != nil {
		return header, err
	}
	if err := binary.Read(buf, binary.BigEndian, &header.Type); err != nil {
		return header, err
	}
	if err := binary.Read(buf, binary.BigEndian, &header.BodyLength); err != nil {
		return header, err
	}
	if err := binary.Read(buf, binary.BigEndian, &header.ReservedWord); err != nil {
		return header, err
	}

	if header.Magic != MagicNumber {
		return header, errors.New("invalid magic number")
	}

	return header, nil
}

func encodeHandshake(msg HandshakeMessage) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, msg.Version)
	binary.Write(buf, binary.BigEndian, msg.ClientID)
	return buf.Bytes()
}

func decodeHandshake(data []byte) (HandshakeMessage, error) {
	var msg HandshakeMessage
	buf := bytes.NewReader(data)
	if err := binary.Read(buf, binary.BigEndian, &msg.Version); err != nil {
		return msg, err
	}
	if err := binary.Read(buf, binary.BigEndian, &msg.ClientID); err != nil {
		return msg, err
	}
	return msg, nil
}

func CalcBlake3(path string) ([32]byte, error) {
	var result [32]byte
	f, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer f.Close()

	hash := blake3.New()
	if _, err := io.Copy(hash, f); err != nil {
		return result, err
	}

	copy(result[:], hash.Sum(nil))
	return result, nil
}

type fileServer struct {
	listenAddr    string
	sessionMap    sync.Map
	nextSessionID uint32
}

type session struct {
	ID       uint32   // 会话ID
	FilePath string   // 文件路径
	FileName string   // 文件名
	FileSize uint64   // 文件大小
	file     *os.File // 文件句柄
}
