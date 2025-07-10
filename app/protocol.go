package app

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"

	log "github.com/sirupsen/logrus"
)

// 协议常量定义
const (
	// 魔术字
	MagicNumber uint32 = 0xF1E2D3C4 // 协议标识符

	// 消息类型
	MsgTypeHandshake     uint16 = 0x0001 // 握手请求/响应
	MsgTypeFileRequest   uint16 = 0x0002 // 文件传输请求
	MsgTypeFileResponse  uint16 = 0x0003 // 文件传输响应
	MsgTypeFileData      uint16 = 0x0004 // 文件数据
	MsgTypeFileComplete  uint16 = 0x0005 // 文件传输完成
	MsgTypeError         uint16 = 0x0006 // 错误消息
	MsgTypeAcknowledge   uint16 = 0x0007 // 确认消息
	MsgTypeTreeRequest   uint16 = 0x0008 // 目录树请求
	MsgTypeTreeResponse  uint16 = 0x0009 // 目录树响应
	MsgTypeHeartbeatPing uint16 = 0x000A // 心跳请求
	MsgTypeHeartbeatPong uint16 = 0x000B // 心跳响应

	// 状态码
	StatusOK            uint16 = 0x0000 // 正常
	StatusReject        uint16 = 0x0001 // 拒绝传输
	StatusFileNotFound  uint16 = 0x0002 // 文件不存在
	StatusInternalError uint16 = 0x0003 // 内部错误
	StatusTreeNotFound  uint16 = 0x0004 // 目录树不存在

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
	Version uint16 // 协议版本
	UUID    uint32 // 标识
	Role    uint8  // 角色
}

// 文件请求消息
type FileRequestMessage struct {
	PathLength uint16 // 文件路径长度
	FilePath   string // 文件路径
	Offset     uint64 // 起始偏移（断点续传用）
}

// 文件响应消息
type FileResponseMessage struct {
	Status    uint16   // 状态码
	SessionID [16]byte // 会话ID
	FileSize  uint64   // 文件大小
	FileHash  [32]byte // 文件哈希值
}

// 文件数据消息
type FileDataMessage struct {
	SessionID  [16]byte // 会话ID
	Offset     uint64   // 数据偏移
	DataLength uint32   // 数据长度
	Data       []byte   // 数据内容
}

// 文件完成消息
type FileCompleteMessage struct {
	SessionID [16]byte // 会话ID
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
	SessionID [16]byte // 会话ID
	Offset    uint64   // 确认偏移
	Status    uint16   // 确认状态
}

// 树形结构请求消息
type TreeRequestMessage struct {
	PathLength uint16 // 路径长度
	RootPath   string // 请求获取的目录树的路径
}

// 树形结构响应消息
type TreeResponseMessage struct {
	Status     uint16 // 状态码
	RootPath   string // 目录树的根路径
	DataLength uint32 // 数据长度
	Data       []byte // 请求数据
}

// 心跳请求消息
type HeartbeatPingMessage struct {
	Version   uint16 // 协议版本
	Timestamp int64  // 时间戳（秒）
	ClientID  uint32 // 客户端标识
}

// 心跳响应消息
type HeartbeatPongMessage struct {
	Version   uint16 // 协议版本
	Timestamp int64  // 时间戳（秒）
	ServerID  uint32 // 服务端标识
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
		return header, fmt.Errorf("invalid magic number, got %d", header.Magic)
	}

	return header, nil
}

func encodeHandshake(msg HandshakeMessage) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, msg.Version)
	binary.Write(buf, binary.BigEndian, msg.UUID)
	binary.Write(buf, binary.BigEndian, msg.Role)
	return buf.Bytes()
}

func decodeHandshake(data []byte) (HandshakeMessage, error) {
	var msg HandshakeMessage
	buf := bytes.NewReader(data)
	if err := binary.Read(buf, binary.BigEndian, &msg.Version); err != nil {
		return msg, err
	}
	if err := binary.Read(buf, binary.BigEndian, &msg.UUID); err != nil {
		return msg, err
	}
	if err := binary.Read(buf, binary.BigEndian, &msg.Role); err != nil {
		return msg, err
	}
	return msg, nil
}

func encodeFileRequest(msg FileRequestMessage) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, msg.PathLength)
	buf.Write([]byte(msg.FilePath))
	binary.Write(buf, binary.BigEndian, msg.Offset)
	return buf.Bytes()
}

func decodeFileRequest(data []byte) (FileRequestMessage, error) {
	var msg FileRequestMessage
	buf := bytes.NewReader(data)
	if err := binary.Read(buf, binary.BigEndian, &msg.PathLength); err != nil {
		log.Error("Error decoding file request message:", err)
		return msg, err
	}
	filePathBytes := make([]byte, msg.PathLength)
	if _, err := buf.Read(filePathBytes); err != nil {
		log.Error("Error reading file name:", err)
		return msg, err
	}
	filePath := string(filePathBytes)
	msg.FilePath = filePath

	if err := binary.Read(buf, binary.BigEndian, &msg.Offset); err != nil {
		log.Error("Error decoding file offset:", err)
		return msg, err
	}
	return msg, nil

}

func encodeFileResponse(msg FileResponseMessage) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, msg.Status)
	buf.Write(msg.SessionID[:])
	binary.Write(buf, binary.BigEndian, msg.FileSize)
	buf.Write(msg.FileHash[:])
	return buf.Bytes()
}

func decodeFileResponse(data []byte) (FileResponseMessage, error) {
	var msg FileResponseMessage
	buf := bytes.NewReader(data)

	if err := binary.Read(buf, binary.BigEndian, &msg.Status); err != nil {
		log.Error("Error decoding file response message:", err)
		return msg, err
	}

	if _, err := buf.Read(msg.SessionID[:]); err != nil {
		log.Error("Error reading session ID:", err)
		return msg, err
	}

	if err := binary.Read(buf, binary.BigEndian, &msg.FileSize); err != nil {
		log.Error("Error decoding file size:", err)
		return msg, err
	}

	if _, err := buf.Read(msg.FileHash[:]); err != nil {
		log.Error("Error reading file hash:", err)
		return msg, err
	}

	return msg, nil
}

func encodeFileData(msg FileDataMessage) []byte {
	buf := new(bytes.Buffer)
	buf.Write(msg.SessionID[:])
	binary.Write(buf, binary.BigEndian, msg.Offset)
	binary.Write(buf, binary.BigEndian, msg.DataLength)
	buf.Write(msg.Data)
	return buf.Bytes()
}

func decodeFileData(data []byte) (FileDataMessage, error) {
	var msg FileDataMessage
	buf := bytes.NewReader(data)

	if _, err := buf.Read(msg.SessionID[:]); err != nil {
		log.Error("Error reading file data session ID:", err)
		return msg, err
	}

	if err := binary.Read(buf, binary.BigEndian, &msg.Offset); err != nil {
		log.Error("Error decoding file data offset:", err)
		return msg, err
	}

	if err := binary.Read(buf, binary.BigEndian, &msg.DataLength); err != nil {
		log.Error("Error decoding file data length:", err)
		return msg, err
	}

	msg.Data = make([]byte, msg.DataLength)
	if _, err := buf.Read(msg.Data); err != nil {
		log.Error("Error reading file data:", err)
		return msg, err
	}
	return msg, nil
}

func encodeAcknowlege(msg AcknowledgeMessage) []byte {
	buf := new(bytes.Buffer)
	buf.Write(msg.SessionID[:])
	binary.Write(buf, binary.BigEndian, msg.Offset)
	binary.Write(buf, binary.BigEndian, msg.Status)
	return buf.Bytes()
}

func decodeAcknowledge(data []byte) (AcknowledgeMessage, error) {
	var msg AcknowledgeMessage
	buf := bytes.NewReader(data)

	if _, err := buf.Read(msg.SessionID[:]); err != nil {
		return msg, fmt.Errorf("error reading acknowledge session ID: %w", err)
	}

	if err := binary.Read(buf, binary.BigEndian, &msg.Offset); err != nil {
		return msg, fmt.Errorf("error decoding acknowledge offset: %w", err)
	}

	if err := binary.Read(buf, binary.BigEndian, &msg.Status); err != nil {
		return msg, fmt.Errorf("error decoding acknowledge status: %w", err)
	}
	return msg, nil
}

func encodeFileComplete(msg FileCompleteMessage) []byte {
	buf := new(bytes.Buffer)
	buf.Write(msg.SessionID[:])
	buf.Write(msg.FileHash[:])
	return buf.Bytes()
}

func decodeFileComplete(data []byte) (FileCompleteMessage, error) {
	var msg FileCompleteMessage
	buf := bytes.NewReader(data)

	if _, err := buf.Read(msg.SessionID[:]); err != nil {
		return msg, fmt.Errorf("error reading file complete session ID: %w", err)
	}
	if _, err := buf.Read(msg.FileHash[:]); err != nil {
		return msg, fmt.Errorf("error reading file complete hash: %w", err)
	}
	return msg, nil
}

func encodeErrorMessage(msg ErrorMessage) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, msg.ErrorCode)
	binary.Write(buf, binary.BigEndian, msg.MessageLen)
	buf.Write([]byte(msg.ErrorMessage))
	return buf.Bytes()
}

func decodeErrorMessage(data []byte) (ErrorMessage, error) {
	var msg ErrorMessage
	buf := bytes.NewReader(data)

	if err := binary.Read(buf, binary.BigEndian, &msg.ErrorCode); err != nil {
		log.Error("Error decoding error message:", err)
		return msg, err
	}
	if err := binary.Read(buf, binary.BigEndian, &msg.MessageLen); err != nil {
		log.Error("Error decoding error message length:", err)
		return msg, err
	}
	errorMessageBytes := make([]byte, msg.MessageLen)
	if _, err := buf.Read(errorMessageBytes); err != nil {
		log.Error("Error reading error message:", err)
		return msg, err
	}
	msg.ErrorMessage = string(errorMessageBytes)
	return msg, nil
}

func sendMessage(conn net.Conn, msgType uint16, body []byte) error {
	header := MessageHeader{
		Magic:        MagicNumber,
		Type:         msgType,
		BodyLength:   uint32(len(body)),
		ReservedWord: 0,
	}

	headerBytes := encodeHeader(header)
	if _, err := conn.Write(headerBytes); err != nil {
		return err
	}
	if _, err := conn.Write(body); err != nil {
		return err
	}
	log.Debugf("Sent message with type: %d, body length: %d bytes", msgType, len(body))
	return nil
}

func receiveMessage(conn net.Conn) (uint16, []byte, error) {
	headerBytes := make([]byte, HeaderSize)
	if _, err := io.ReadFull(conn, headerBytes); err != nil {
		return 0, nil, err
	}

	header, err := decodeHeader(headerBytes)
	if err != nil {
		remoteAddr := conn.RemoteAddr().String()
		_error := fmt.Errorf("error decoding message header from %s: %w", remoteAddr, err)
		return 0, nil, _error
	}

	bodyBytes := make([]byte, header.BodyLength)
	if header.BodyLength > 0 {
		if _, err := io.ReadFull(conn, bodyBytes); err != nil {
			return 0, nil, err
		}
	}

	return header.Type, bodyBytes, nil
}

func encodeTreeRequest(msg TreeRequestMessage) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, msg.PathLength)
	buf.Write([]byte(msg.RootPath))
	return buf.Bytes()
}

func decodeTreeRequest(data []byte) (TreeRequestMessage, error) {
	var msg TreeRequestMessage
	buf := bytes.NewReader(data)

	if err := binary.Read(buf, binary.BigEndian, &msg.PathLength); err != nil {
		log.Error("Error decoding tree request path length:", err)
		return msg, err
	}
	pathBytes := make([]byte, msg.PathLength)
	if _, err := buf.Read(pathBytes); err != nil {
		log.Error("Error reading tree request path:", err)
		return msg, err
	}
	msg.RootPath = string(pathBytes)
	return msg, nil
}

func encodeTreeResponse(msg TreeResponseMessage) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, msg.Status)
	pathBytes := []byte(msg.RootPath)
	binary.Write(buf, binary.BigEndian, uint16(len(pathBytes)))
	buf.Write(pathBytes)
	binary.Write(buf, binary.BigEndian, msg.DataLength)
	buf.Write(msg.Data)
	return buf.Bytes()
}

func decodeTreeResponse(data []byte) (TreeResponseMessage, error) {
	var msg TreeResponseMessage
	buf := bytes.NewReader(data)

	if err := binary.Read(buf, binary.BigEndian, &msg.Status); err != nil {
		log.Error("Error decoding tree response status:", err)
		return msg, err
	}

	var pathLength uint16
	if err := binary.Read(buf, binary.BigEndian, &pathLength); err != nil {
		log.Error("Error decoding tree response path length:", err)
		return msg, err
	}

	pathBytes := make([]byte, pathLength)
	if _, err := buf.Read(pathBytes); err != nil {
		log.Error("Error reading tree response path:", err)
		return msg, err
	}
	msg.RootPath = string(pathBytes)

	if err := binary.Read(buf, binary.BigEndian, &msg.DataLength); err != nil {
		log.Error("Error decoding tree response data length:", err)
		return msg, err
	}

	msg.Data = make([]byte, msg.DataLength)
	if _, err := buf.Read(msg.Data); err != nil {
		log.Error("Error reading tree response data:", err)
		return msg, err
	}

	return msg, nil
}

func encodeHeartbeatPing(msg HeartbeatPingMessage) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, msg.Version)
	binary.Write(buf, binary.BigEndian, msg.Timestamp)
	binary.Write(buf, binary.BigEndian, msg.ClientID)
	return buf.Bytes()
}

func decodeHeartbeatPing(data []byte) (HeartbeatPingMessage, error) {
	var msg HeartbeatPingMessage
	buf := bytes.NewReader(data)

	if err := binary.Read(buf, binary.BigEndian, &msg.Version); err != nil {
		log.Error("Error decoding heartbeat ping version:", err)
		return msg, err
	}
	if err := binary.Read(buf, binary.BigEndian, &msg.Timestamp); err != nil {
		log.Error("Error decoding heartbeat ping second:", err)
		return msg, err
	}
	if err := binary.Read(buf, binary.BigEndian, &msg.ClientID); err != nil {
		log.Error("Error decoding heartbeat ping client ID:", err)
		return msg, err
	}
	return msg, nil
}

func encodeHeartbeatPong(msg HeartbeatPongMessage) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, msg.Version)
	binary.Write(buf, binary.BigEndian, msg.Timestamp)
	binary.Write(buf, binary.BigEndian, msg.ServerID)
	return buf.Bytes()
}

func decodeHeartbeatPong(data []byte) (HeartbeatPongMessage, error) {
	var msg HeartbeatPongMessage
	buf := bytes.NewReader(data)

	if err := binary.Read(buf, binary.BigEndian, &msg.Version); err != nil {
		log.Error("Error decoding heartbeat pong version:", err)
		return msg, err
	}
	if err := binary.Read(buf, binary.BigEndian, &msg.Timestamp); err != nil {
		log.Error("Error decoding heartbeat pong second:", err)
		return msg, err
	}
	if err := binary.Read(buf, binary.BigEndian, &msg.ServerID); err != nil {
		log.Error("Error decoding heartbeat pong server ID:", err)
		return msg, err
	}
	return msg, nil
}
