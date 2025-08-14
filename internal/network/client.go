package network

import (
	"bytes"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/pkg/utils"
	"net"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
)

var (
	Waiting    uint8 = 0x00 // 等待
	Online     uint8 = 0x01 // 在线
	Offline    uint8 = 0x02 // 离线
	Deprecated uint8 = 0x03 // 废弃
)

type FileClient struct {
	RealityAddr      string
	Alias            string
	connectionManage *utils.ConnectionManager
	realityVersion   uint16
	realityID        uint32
	State            uint8
}

func NewFileClient(realityAddr string, serverAlias string) (*FileClient, error) {
	log.Info("Creating file client, server address:", realityAddr)
	connetion, err := utils.NewConnectionManager(realityAddr)
	if err != nil {
		return &FileClient{
			RealityAddr:      realityAddr,
			Alias:            serverAlias,
			connectionManage: nil,
			State:            Offline,
		}, fmt.Errorf("failed to create connection manager: %w", err)
	}
	return &FileClient{
		RealityAddr:      realityAddr,
		Alias:            serverAlias,
		connectionManage: connetion,
		State:            Waiting,
	}, nil
}

func (c *FileClient) ConnectionClose() {
	if c.connectionManage != nil {
		c.connectionManage.Close()
	}
}

func (c *FileClient) Reconnect() error {
	log.Warnf("Reconnecting to server at %s", c.RealityAddr)
	if err := c.connectionManage.Reconnect(); err != nil {
		return fmt.Errorf("failed to reconnect: %w", err)
	}
	c.State = Waiting
	err := c.Reverify()
	if err != nil {
		c.State = Deprecated
		c.connectionManage.Close()
		log.Errorf("Reverification failed, Abandon this client: %v", err)
	}
	log.Info("Reconnected successfully")
	return nil
}

func (c *FileClient) Reverify() error {
	conn, _ := c.connectionManage.GetConnection()
	if err := sendMessage(conn, MsgTypeHandshake, nil); err != nil {
		return fmt.Errorf("failed to send reverify message: %w", err)
	}
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		return fmt.Errorf("failed to receive reverify response: %w", err)
	}
	if msgType != MsgTypeReverifyResponse {
		return fmt.Errorf("invalid reverify response message type, got %d", msgType)
	}
	reverifyResponse, err := decodeReverifyResponse(bodyBytes)
	if err != nil {
		return fmt.Errorf("failed to decode reverify response: %w", err)
	}
	if reverifyResponse.Version != config.ProtocolVersion || reverifyResponse.ServerID != config.InstanceID {
		return fmt.Errorf("reverify failed, expected version %d and server ID %d, got version %d and server ID %d",
			c.realityVersion, c.realityID,
			reverifyResponse.Version, reverifyResponse.ServerID)
	}
	return nil
}

func (c *FileClient) Handshake() error {
	conn, err := c.connectionManage.GetConnection()
	if err != nil {
		return fmt.Errorf("failed to get connection: %w", err)
	}
	handshakeMsg := HandshakeMessage{
		Version: config.ProtocolVersion,
		UUID:    config.InstanceID,
		Role:    config.ModeMap[*config.Mode],
	}
	handshakeBytes := encodeHandshake(handshakeMsg)

	if err := sendMessage(conn, MsgTypeHandshake, handshakeBytes); err != nil {
		return fmt.Errorf("failed to send handshake message: %w", err)
	}
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		return fmt.Errorf("failed to receive message: %w", err)
	}

	if msgType != MsgTypeHandshake {
		return fmt.Errorf("invalid handshake response message type, got %d", msgType)
	}
	handshakeResponse, err := decodeHandshake(bodyBytes)
	if err != nil {
		return fmt.Errorf("failed to decode handshake:  %w", err)
	}
	c.realityVersion = handshakeResponse.Version
	c.realityID = handshakeResponse.UUID
	c.State = Online
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
	if err := sendMessage(conn, MsgTypeHeartbeatPing, pingBytes); err != nil {
		log.Error("Error sending ping message:", err)
	}
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		return fmt.Errorf("failed to receive message: %w", err)
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
	if err != nil {
		return nil, fmt.Errorf("%w, failed to get connection: %w", appError.ErrConnection, err)
	}
	request := TreeRequestMessage{
		PathLength: uint16(len(rootPath)),
		RootPath:   rootPath,
	}
	requestBytes := encodeTreeRequest(request)
	realityAddr := conn.RemoteAddr().String()
	if err := sendMessage(conn, MsgTypeTreeRequest, requestBytes); err != nil {
		return nil, fmt.Errorf("%w, failed to send tree request: %w", appError.ErrConnection, err)
	}
	log.Debugf("Sent tree request to %s for path: %s", realityAddr, rootPath)
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		return nil, fmt.Errorf("%w, failed to receive message: %w", appError.ErrConnection, err)
	}
	if msgType == MsgTypeError {
		errorMsg, err := decodeErrorMessage(bodyBytes)
		if err != nil {
			return nil, fmt.Errorf("%w, failed to decode error message: %w", appError.ErrConnection, err)
		}

		return nil, fmt.Errorf("reality error: %s", errorMsg.ErrorMessage)
	}
	if msgType != MsgTypeTreeResponse {
		return nil, fmt.Errorf("invalid tree response message type, got %d", msgType)
	}
	treeResponse, err := decodeTreeResponse(bodyBytes)
	if err != nil {
		return nil, fmt.Errorf("%w, failed to decode tree response: %w", appError.ErrConnection, err)
	}

	if len(treeResponse.Data) == 0 {
		log.Warnf("Received empty tree response from %s, path: %s", realityAddr, rootPath)
		return []byte{}, nil
	}
	log.Infof("Received tree response from %s, data length: %d bytes",
		realityAddr,
		len(treeResponse.Data))
	log.Debugf("Received tree response: %s", treeResponse.Data)
	return treeResponse.Data, nil
}

func (c *FileClient) DownloadFile(filePath string) (string, error) {
	conn, err := c.connectionManage.GetConnection()
	if err != nil {
		return "", fmt.Errorf("%w, failed to get connection: %w", appError.ErrConnection, err)
	}
	requestFile := FileRequestMessage{
		PathLength: uint16(len(filePath)),
		FilePath:   filePath,
		Offset:     0,
	}
	requestBytes := encodeFileRequest(requestFile)
	if err := sendMessage(conn, MsgTypeFileRequest, requestBytes); err != nil {
		return "", fmt.Errorf("%w, failed to send file request: %w", appError.ErrConnection, err)
	}

	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		return "", fmt.Errorf("%w, failed to receive message: %w", appError.ErrConnection, err)
	}

	if msgType == MsgTypeError {
		errorMsg, err := decodeErrorMessage(bodyBytes)
		if err != nil {
			return "", fmt.Errorf("%w, failed to decode error message: %w", appError.ErrConnection, err)
		}
		return "", fmt.Errorf("reality error: %s", errorMsg.ErrorMessage)
	}

	if msgType != MsgTypeFileResponse {
		return "", fmt.Errorf("invalid file response message type, got %d", msgType)
	}
	fileResponse, err := decodeFileResponse(bodyBytes)
	if err != nil {
		return "", fmt.Errorf("%w, failed to decode file response: %w", appError.ErrConnection, err)
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
			return "", fmt.Errorf("%w, failed to receive message: %w", appError.ErrConnection, err)
		}
		switch msgType {
		case MsgTypeFileData:
			dataMsg, err := decodeFileData(bodyBytes)
			if err != nil {
				return "", fmt.Errorf("%w, error decoding file data message: %w", appError.ErrConnection, err)
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
			if err := sendMessage(conn, MsgTypeAcknowledge, ackBytes); err != nil {
				return "", fmt.Errorf("%w, failed to send acknowledge message: %w", appError.ErrConnection, err)
			}
			log.Debugf("Sent acknowledge message, session ID: %s, offset: %d", sessionID, receivedSize)
		case MsgTypeFileComplete:
			completeMsg, err := decodeFileComplete(bodyBytes)
			if err != nil {
				return "", fmt.Errorf("%w, error decoding file complete message: %w", appError.ErrConnection, err)
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
			return "", fmt.Errorf("reality error: %s", errorMsg.ErrorMessage)
		default:
			return "", fmt.Errorf("invalid file data message type, got %d", msgType)
		}
	}
}
