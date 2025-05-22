package app

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"local-mirror/config"
	"net"
	"os"
	"path/filepath"
	"sync"

	log "github.com/sirupsen/logrus"
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
	ServerID uint32 // 服务端标识
	ClientID uint32 // 客户端标识
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
	binary.Write(buf, binary.BigEndian, msg.ServerID)
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
	if err := binary.Read(buf, binary.BigEndian, &msg.ServerID); err != nil {
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
	binary.Write(buf, binary.BigEndian, msg.SessionID)
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

	if err := binary.Read(buf, binary.BigEndian, &msg.SessionID); err != nil {
		log.Error("Error decoding session ID:", err)
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
	binary.Write(buf, binary.BigEndian, msg.SessionID)
	binary.Write(buf, binary.BigEndian, msg.Offset)
	binary.Write(buf, binary.BigEndian, msg.DataLength)
	buf.Write(msg.Data)
	return buf.Bytes()
}

func decodeFileData(data []byte) (FileDataMessage, error) {
	var msg FileDataMessage
	buf := bytes.NewReader(data)

	if err := binary.Read(buf, binary.BigEndian, &msg.SessionID); err != nil {
		log.Error("Error decoding file data message:", err)
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
	return nil
}

func receiveMessage(conn net.Conn) (uint16, []byte, error) {
	headerBytes := make([]byte, HeaderSize)
	if _, err := io.ReadFull(conn, headerBytes); err != nil {
		return 0, nil, err
	}
	header, err := decodeHeader(headerBytes)
	if err != nil {
		return 0, nil, err
	}
	bodyBytes := make([]byte, header.BodyLength)
	if header.BodyLength > 0 {
		if _, err := io.ReadFull(conn, bodyBytes); err != nil {
			return 0, nil, err
		}
	}

	return header.Type, bodyBytes, nil
}

func NewFileServer(listenAddr string) *fileServer {
	log.Debug("Creating file server, listen address:", listenAddr)
	return &fileServer{
		listenAddr:    listenAddr,
		nextSessionID: 1,
	}
}

func (s *fileServer) Start() error {
	listener, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		log.Errorf("Error starting server: %v", err)
	}
	log.Infof("File server started on %s", s.listenAddr)
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Error("Error accepting connection:", err)
			continue
		}
		go s.handleConnection(conn)
	}
}

func (s *fileServer) handleConnection(conn net.Conn) {
	defer conn.Close()

	clientAddr := conn.RemoteAddr().String()
	log.Infof("Client connected: %s", clientAddr)

	if err := s.handleHandshake(conn); err != nil {
		log.Error("Error during handshake:", err)
		return
	}

	for {
		msgType, bodyBytes, err := receiveMessage(conn)
		fmt.Printf("Received message type %d with body size %d\n", msgType, binary.Size(bodyBytes))
		if err != nil {
			if err != io.EOF {
				log.Errorf("Error receiving message from %s, %v\n", clientAddr, err)
			} else {
				log.Infof("Client %s disconnected", clientAddr)
			}
			break
		}

		switch msgType {

		}

	}

}

func (s *fileServer) handleHandshake(conn net.Conn) error {
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		log.Error("Error receiving message:", err)
		return err
	}
	if msgType != MsgTypeHandshake {
		log.Error("Invalid message type:", msgType)
		return fmt.Errorf("invalid message type, got message type %d", msgType)
	}

	handshakeMsg, err := decodeHandshake(bodyBytes)
	if err != nil {
		log.Error("Error decoding handshake message:", err)
		return err
	}
	log.Infof("Received handshake message: version: %d, clientID: %d, serverID: %d",
		handshakeMsg.Version,
		handshakeMsg.ClientID,
		handshakeMsg.ServerID)
	receiveHandshake := HandshakeMessage{
		Version:  config.Version,
		ServerID: config.InstanceID,
		ClientID: 0,
	}
	handshakeBytes := encodeHandshake(receiveHandshake)
	if err := sendMessage(conn, MsgTypeHandshake, handshakeBytes); err != nil {
		log.Error("Error sending handshake message:", err)
		return err
	}
	return nil
}

type fileClient struct {
	serverAddr string
}

func NewFileClient(serverAddr string) *fileClient {
	log.Debug("Creating file client, server address:", serverAddr)
	return &fileClient{
		serverAddr: serverAddr,
	}
}

func (c *fileClient) Connect() (net.Conn, error) {
	conn, err := net.Dial("tcp", c.serverAddr)
	if err != nil {
		log.Errorf("Error connecting to server %s: %v", c.serverAddr, err)
		return nil, err
	}
	log.Infof("Connected to server %s", c.serverAddr)

	handshakeMsg := HandshakeMessage{
		Version:  config.Version,
		ServerID: 0,
		ClientID: config.InstanceID,
	}
	handshakeBytes := encodeHandshake(handshakeMsg)

	if err := sendMessage(conn, MsgTypeHandshake, handshakeBytes); err != nil {
		log.Error("Error sending handshake message:", err)
		return nil, err
	}
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		log.Error("Error receiving handshake response:", err)
		conn.Close()
		return nil, err
	}

	if msgType != MsgTypeHandshake {
		newError := fmt.Errorf("invalid handshake response message type, got %d", msgType)
		log.Error(newError)
		conn.Close()
		return nil, newError
	}
	handshakeResponse, err := decodeHandshake(bodyBytes)
	if err != nil {
		log.Error("Error decoding handshake response:", err)
		conn.Close()
		return nil, err
	}
	log.Infof("Received handshake response: version: %d, clientID: %d, serverID: %d",
		handshakeResponse.Version,
		handshakeResponse.ClientID,
		handshakeResponse.ServerID)
	return conn, nil
}

func (c *fileClient) DownloadFile(conn net.Conn, filePath string) error {
	requestFile := FileRequestMessage{
		PathLength: uint16(len(filePath)),
		FilePath:   filePath,
		Offset:     0,
	}
	requestBytes := encodeFileRequest(requestFile)
	if err := sendMessage(conn, MsgTypeFileRequest, requestBytes); err != nil {
		log.Error("Error sending file request message:", err)
		return err
	}

	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		log.Error("Error receiving file response:", err)
		return err
	}

	if msgType == MsgTypeError {
		errorMsg, err := decodeErrorMessage(bodyBytes)
		if err != nil {
			log.Error("Error decoding error message:", err)
			return err
		}
		newError := fmt.Errorf("server error: %s", errorMsg.ErrorMessage)
		log.Error(newError)
		return newError
	}

	if msgType != MsgTypeFileResponse {
		newError := fmt.Errorf("invalid file response message type, got %d", msgType)
		log.Error(newError)
		return newError
	}
	fileResponse, err := decodeFileResponse(bodyBytes)
	if err != nil {
		return err
	}
	if fileResponse.Status != StatusOK {
		newError := fmt.Errorf("file transfer rejected, status code: %d", fileResponse.Status)
		log.Error(newError)
		return newError
	}

	fullPath := filepath.Join(config.StartPath, filePath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		log.Error("Error creating directory:", err)
		return err
	}
	file, err := os.Create(fullPath)
	if err != nil {
		log.Error("Error creating file:", err)
	}
	defer file.Close()
	sessionID := fileResponse.SessionID
	receivedSize := uint64(0)
	startTime := time.Now()

	if fileResponse.FileSize <= 0 {
		file.Close()
		log.Debug("create empty file:", fullPath)
	} else if fileResponse.FileSize > 0 {
		for {
			msgType, bodyBytes, err := receiveMessage(conn)
			if err != nil {
				log.Error("Error receiving file data:", err)
				return err
			}
			switch msgType {
				dataMsg, err := decodeFileData(bodyBytes)
			}
		}
	}

}
