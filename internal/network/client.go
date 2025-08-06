package network

import (
	"bytes"
	"fmt"
	"local-mirror/config"
	"local-mirror/pkg/utils"
	"net"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
)

var (
	UnConnected     uint8 = 0x00 // 未连接
	Connected       uint8 = 0x01 // 已连接
	HandshakSuccess uint8 = 0x02 // 握手成功
	HandshakeError  uint8 = 0x03 // 握手失败
)

var (
	Waiting uint8 = 0x00 // 等待
	Online  uint8 = 0x01 // 在线
	Offline uint8 = 0x02 // 离线
)

type FileClient struct {
	RealityAddr      string
	Alias            string
	connectionManage *utils.ConnectionManager
	realityVersion   uint16
	realityID        uint32
	State            uint8
	Mode             uint8
}

func NewFileClient(realityAddr string, serverAlias string) *FileClient {
	log.Info("Creating file client, server address:", realityAddr)
	return &FileClient{
		RealityAddr:      realityAddr,
		Alias:            serverAlias,
		connectionManage: utils.NewConnectionManager(realityAddr),
		State:            UnConnected,
		Mode:             Waiting,
	}
}

func (c *FileClient) ConnectionClose() {
	c.State = UnConnected
	if c.connectionManage != nil {
		c.connectionManage.Close()
	}
}

func (c *FileClient) Handshake() error {
	conn, err := c.connectionManage.GetConnection()
	if err != nil {
		return fmt.Errorf("failed to get connection: %w", err)
	}
	c.State = Connected
	handshakeMsg := HandshakeMessage{
		Version: config.ProtocolVersion,
		UUID:    config.InstanceID,
		Role:    config.ModeMap[*config.Mode],
	}
	handshakeBytes := encodeHandshake(handshakeMsg)

	if err := sendMessage(conn, MsgTypeHandshake, StatusOK, handshakeBytes); err != nil {
		c.State = HandshakeError
		return fmt.Errorf("failed to send handshake message: %w", err)
	}
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		c.State = HandshakeError
		return fmt.Errorf("receive message: %w", err)
	}

	if msgType != MsgTypeHandshake {
		c.State = HandshakeError
		return fmt.Errorf("invalid handshake response message type, got %d", msgType)
	}
	handshakeResponse, err := decodeHandshake(bodyBytes)
	if err != nil {
		c.State = HandshakeError
		return fmt.Errorf("failed to decode handshake response: %w", err)
	}
	c.realityVersion = handshakeResponse.Version
	c.realityID = handshakeResponse.UUID
	c.State = HandshakSuccess
	c.Mode = Online
	log.Infof("Received handshake response: version: %d, realityID: %d",
		handshakeResponse.Version,
		handshakeResponse.UUID)
	return nil
}

func (c *FileClient) Ping(conn net.Conn) error {
	pingMessage := HeartbeatPingMessage{
		Version:   config.ProtocolVersion,
		Timestamp: time.Now().Unix(),
		ClientID:  config.InstanceID,
	}
	pingBytes := encodeHeartbeatPing(pingMessage)
	if err := sendMessage(conn, MsgTypeHeartbeatPing, StatusOK, pingBytes); err != nil {
		log.Error("Error sending ping message:", err)
	}
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		log.Error("Error receiving pong:", err)
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
	if msgType != MsgTypeHeartbeatPong {
		newError := fmt.Errorf("invalid pong message type, got %d", msgType)
		log.Error(newError)
		return newError
	}
	pongMessage, err := decodeHeartbeatPong(bodyBytes)
	if err != nil {
		log.Error("Error decoding pong message:", err)
	}
	log.Infof("Received pong message: version: %d, timestamp: %d, clientID: %d",
		pongMessage.Version,
		pongMessage.Timestamp,
		pongMessage.ServerID)
	return nil
}

func (c *FileClient) GetRealityTree(rootPath string) ([]byte, error) {
	conn, err := c.connectionManage.GetConnection()
	request := TreeRequestMessage{
		PathLength: uint16(len(rootPath)),
		RootPath:   rootPath,
	}
	requestBytes := encodeTreeRequest(request)
	realityAddr := conn.RemoteAddr().String()
	if err := sendMessage(conn, MsgTypeTreeRequest, StatusOK, requestBytes); err != nil {
		return nil, fmt.Errorf("failed to send tree request: %w", err)
	}
	log.Debugf("Sent tree request to %s for path: %s", realityAddr, rootPath)
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		return nil, fmt.Errorf("failed to receive tree response: %w", err)
	}
	if msgType == MsgTypeError {
		errorMsg, err := decodeErrorMessage(bodyBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to decode error message: %w", err)
		}

		return nil, fmt.Errorf("[server error]: %s", errorMsg.ErrorMessage)
	}
	if msgType != MsgTypeTreeResponse {
		return nil, fmt.Errorf("invalid tree response message type, got %d", msgType)
	}
	treeResponse, err := decodeTreeResponse(bodyBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to decode tree response: %w", err)
	}

	if len(treeResponse.Data) == 0 {
		log.Warnf("Received empty tree response from %s, path: %s", realityAddr, rootPath)
		return []byte{}, nil
	}
	log.Infof("Received tree response from %s, status: %d, data length: %d bytes",
		realityAddr,
		len(treeResponse.Data))
	log.Debugf("Received tree response: %s", treeResponse.Data)
	return treeResponse.Data, nil
}

func (c *FileClient) DownloadFile(filePath string) (string, error) {
	conn, err := c.connectionManage.GetConnection()
	if err != nil {
		return "", fmt.Errorf("failed to get connection: %w", err)
	}
	requestFile := FileRequestMessage{
		PathLength: uint16(len(filePath)),
		FilePath:   filePath,
		Offset:     0,
	}
	requestBytes := encodeFileRequest(requestFile)
	if err := sendMessage(conn, MsgTypeFileRequest, StatusOK, requestBytes); err != nil {
		return "", fmt.Errorf("failed to send file request: %w", err)
	}

	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		return "", fmt.Errorf("failed to receive file response: %w", err)
	}

	if msgType == MsgTypeError {
		errorMsg, err := decodeErrorMessage(bodyBytes)
		if err != nil {
			return "", fmt.Errorf("failed to decode error message: %w", err)
		}
		return "", fmt.Errorf("server error: %s", errorMsg.ErrorMessage)
	}

	if msgType != MsgTypeFileResponse {
		return "", fmt.Errorf("invalid file response message type, got %d", msgType)
	}
	fileResponse, err := decodeFileResponse(bodyBytes)
	if err != nil {
		return "", fmt.Errorf("failed to decode file response: %w", err)
	}
	if fileResponse.Status != StatusOK {
		return "", fmt.Errorf("file transfer rejected, status code: %d", fileResponse.Status)
	}

	fullPath := filepath.Join(config.StartPath, filePath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create directory for file: %w", err)
	}
	file, err := os.Create(fullPath)
	if err != nil {
		return "", fmt.Errorf("failed to create file %w", err)
	}
	defer file.Close()
	sessionID := fileResponse.SessionID
	receivedSize := uint64(0)
	startTime := time.Now()
	cacheFile := new(bytes.Buffer)

	for {
		msgType, bodyBytes, err := receiveMessage(conn)
		if err != nil {
			return "", fmt.Errorf("error receiving file data: %w", err)
		}
		switch msgType {
		case MsgTypeFileData:
			dataMsg, err := decodeFileData(bodyBytes)
			if err != nil {
				return "", fmt.Errorf("error decoding file data message: %w", err)
			}
			// todo: check session ID
			if dataMsg.SessionID != sessionID {
				return "", fmt.Errorf("invalid session ID in file data message, got %s", dataMsg.SessionID)
			}

			if fileResponse.FileSize <= *config.MemFileThreshold {
				if _, err := cacheFile.Write(dataMsg.Data); err != nil {
					return "", fmt.Errorf("error writing cached file data: %w", err)
				}
			} else {
				if _, err := file.Write(dataMsg.Data); err != nil {
					return "", fmt.Errorf("error writing file data: %w", err)
				}
			}
			receivedSize += uint64(len(dataMsg.Data))
			ackMsg := AcknowledgeMessage{
				SessionID: sessionID,
				Offset:    receivedSize,
			}

			ackBytes := encodeAcknowlege(ackMsg)
			if err := sendMessage(conn, MsgTypeAcknowledge, StatusOK, ackBytes); err != nil {
				return "", fmt.Errorf("failed to send acknowledge message: %w", err)
			}
			log.Debugf("Sent acknowledge message, session ID: %s, offset: %d", sessionID, receivedSize)
		case MsgTypeFileComplete:
			completeMsg, err := decodeFileComplete(bodyBytes)
			if err != nil {
				return "", fmt.Errorf("error decoding file complete message: %w", err)
			}
			if completeMsg.SessionID != sessionID {
				return "", fmt.Errorf("invalid session ID in file complete message, got %s", completeMsg.SessionID)
			}
			if fileResponse.FileSize <= *config.MemFileThreshold {
				if _, err := file.Write(cacheFile.Bytes()); err != nil {
					return "", fmt.Errorf("error writing cached file data: %w", err)
				}
				cacheFile.Reset()
			}

			if err := file.Close(); err != nil {
				log.Error("Error closing file:", err)
				return "", err
			}

			file.Sync()
			fileHash, err := utils.CalcBlake3(fullPath)
			if err != nil {
				return "", fmt.Errorf("error calculating file hash: %w", err)
			}
			if fileHash != completeMsg.FileHash {
				os.Remove(fullPath)
				return "", fmt.Errorf("file hash mismatch, expected %s, got %s", completeMsg.FileHash, fileHash)
			}
			transferSpeed := float64(fileResponse.FileSize) / time.Since(startTime).Seconds()
			log.Infof("File transfer complete, file path: %s, file size: %d bytes, transfer speed: %.2f MB/s",
				fullPath,
				fileResponse.FileSize,
				transferSpeed/1024/1024)
			return fmt.Sprintf("%x", fileHash), nil
		case MsgTypeError:
			errorMsg, err := decodeErrorMessage(bodyBytes)
			if err != nil {
				return "", fmt.Errorf("error decoding error message: %w", err)
			}
			return "", fmt.Errorf("server error: %s", errorMsg.ErrorMessage)
		default:
			return "", fmt.Errorf("invalid file data message type, got %d", msgType)
		}
	}
}
