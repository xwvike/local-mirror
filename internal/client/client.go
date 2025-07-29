package client

import (
	"bytes"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/protocol"
	"net"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
)

type FileClient struct {
	serverAddr string
}

func NewFileClient(serverAddr string) *FileClient {
	log.Info("Creating file client, server address:", serverAddr)
	return &FileClient{
		serverAddr: serverAddr,
	}
}

func (c *FileClient) Connect() (net.Conn, error) {
	conn, err := net.Dial("tcp", c.serverAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to server %s: %w", c.serverAddr, err)
	}
	log.Infof("Connected to server %s", c.serverAddr)

	handshakeMsg := protocol.HandshakeMessage{
		Version: config.Version,
		UUID:    config.InstanceID,
		Role:    config.ModeMap[*config.Mode],
	}
	handshakeBytes := protocol.EncodeHandshake(handshakeMsg)

	if err := protocol.SendMessage(conn, protocol.MsgTypeHandshake, handshakeBytes); err != nil {
		return nil, fmt.Errorf("failed to send handshake message: %w", err)
	}
	msgType, bodyBytes, err := protocol.ReceiveMessage(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to receive handshake response: %w", err)
	}

	if msgType != protocol.MsgTypeHandshake {
		conn.Close()
		return nil, fmt.Errorf("invalid handshake response message type, got %d", msgType)
	}
	handshakeResponse, err := protocol.DecodeHandshake(bodyBytes)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to decode handshake response: %w", err)
	}
	log.Infof("Received handshake response: version: %d, clientID: %d",
		handshakeResponse.Version,
		handshakeResponse.UUID)
	return conn, nil
}

func (c *FileClient) Ping(conn net.Conn) error {
	pingMessage := protocol.HeartbeatPingMessage{
		Version:   config.Version,
		Timestamp: time.Now().Unix(),
		ClientID:  config.InstanceID,
	}
	pingBytes := protocol.EncodeHeartbeatPing(pingMessage)
	if err := protocol.SendMessage(conn, protocol.MsgTypeHeartbeatPing, pingBytes); err != nil {
		log.Error("Error sending ping message:", err)
	}
	msgType, bodyBytes, err := protocol.ReceiveMessage(conn)
	if err != nil {
		log.Error("Error receiving pong:", err)
		return err
	}
	if msgType == protocol.MsgTypeError {
		errorMsg, err := protocol.DecodeErrorMessage(bodyBytes)
		if err != nil {
			log.Error("Error decoding error message:", err)
			return err
		}
		newError := fmt.Errorf("server error: %s", errorMsg.ErrorMessage)
		log.Error(newError)
		return newError
	}
	if msgType != protocol.MsgTypeHeartbeatPong {
		newError := fmt.Errorf("invalid pong message type, got %d", msgType)
		log.Error(newError)
		return newError
	}
	pongMessage, err := protocol.DecodeHeartbeatPong(bodyBytes)
	if err != nil {
		log.Error("Error decoding pong message:", err)
	}
	log.Infof("Received pong message: version: %d, timestamp: %d, clientID: %d",
		pongMessage.Version,
		pongMessage.Timestamp,
		pongMessage.ServerID)
	return nil
}

func (c *FileClient) GetRealityTree(conn net.Conn, rootPath string) ([]byte, error) {
	request := protocol.TreeRequestMessage{
		PathLength: uint16(len(rootPath)),
		RootPath:   rootPath,
	}
	requestBytes := protocol.EncodeTreeRequest(request)
	serverAddr := conn.RemoteAddr().String()
	if err := protocol.SendMessage(conn, protocol.MsgTypeTreeRequest, requestBytes); err != nil {
		return nil, fmt.Errorf("failed to send tree request: %w", err)
	}
	log.Debugf("Sent tree request to %s for path: %s", serverAddr, rootPath)
	msgType, bodyBytes, err := protocol.ReceiveMessage(conn)
	if err != nil {
		return nil, fmt.Errorf("failed to receive tree response: %w", err)
	}
	if msgType == protocol.MsgTypeError {
		errorMsg, err := protocol.DecodeErrorMessage(bodyBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to decode error message: %w", err)
		}

		return nil, fmt.Errorf("[server error]: %s", errorMsg.ErrorMessage)
	}

	if msgType != protocol.MsgTypeTreeResponse {
		return nil, fmt.Errorf("invalid tree response message type, got %d", msgType)
	}
	treeResponse, err := protocol.DecodeTreeResponse(bodyBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to decode tree response: %w", err)
	}

	if treeResponse.Status != protocol.StatusOK {
		return nil, fmt.Errorf("tree request failed, status code: %d", treeResponse.Status)
	}
	if len(treeResponse.Data) == 0 {
		log.Warnf("Received empty tree response from %s, path: %s", serverAddr, rootPath)
		return []byte{}, nil
	}
	log.Infof("Received tree response from %s, status: %d, data length: %d bytes",
		serverAddr,
		treeResponse.Status,
		len(treeResponse.Data))
	log.Debugf("Received tree response: %s", treeResponse.Data)
	return treeResponse.Data, nil
}

func (c *FileClient) DownloadFile(conn net.Conn, filePath string) (string, error) {
	requestFile := protocol.FileRequestMessage{
		PathLength: uint16(len(filePath)),
		FilePath:   filePath,
		Offset:     0,
	}
	requestBytes := protocol.EncodeFileRequest(requestFile)
	if err := protocol.SendMessage(conn, protocol.MsgTypeFileRequest, requestBytes); err != nil {
		return "", fmt.Errorf("failed to send file request: %w", err)
	}

	msgType, bodyBytes, err := protocol.ReceiveMessage(conn)
	if err != nil {
		return "", fmt.Errorf("failed to receive file response: %w", err)
	}

	if msgType == protocol.MsgTypeError {
		errorMsg, err := protocol.DecodeErrorMessage(bodyBytes)
		if err != nil {
			return "", fmt.Errorf("failed to decode error message: %w", err)
		}
		return "", fmt.Errorf("server error: %s", errorMsg.ErrorMessage)
	}

	if msgType != protocol.MsgTypeFileResponse {
		return "", fmt.Errorf("invalid file response message type, got %d", msgType)
	}
	fileResponse, err := protocol.DecodeFileResponse(bodyBytes)
	if err != nil {
		return "", fmt.Errorf("failed to decode file response: %w", err)
	}
	if fileResponse.Status != protocol.StatusOK {
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
		msgType, bodyBytes, err := protocol.ReceiveMessage(conn)
		if err != nil {
			return "", fmt.Errorf("error receiving file data: %w", err)
		}

		switch msgType {
		case protocol.MsgTypeFileData:
			dataMsg, err := protocol.DecodeFileData(bodyBytes)
			if err != nil {
				return "", fmt.Errorf("error decoding file data: %w", err)
			}
			if dataMsg.SessionID != sessionID {
				return "", fmt.Errorf("invalid session ID in file data, got %s", dataMsg.SessionID)
			}

			receivedSize += uint64(dataMsg.DataLength)
			if fileResponse.FileSize <= *config.MemFileThreshold {
				cacheFile.Write(dataMsg.Data)
			} else {
				if _, err := file.Write(dataMsg.Data); err != nil {
					return "", fmt.Errorf("error writing file data: %w", err)
				}
			}

			log.Debugf("Received %d bytes, total: %d/%d bytes",
				dataMsg.DataLength, receivedSize, fileResponse.FileSize)

			ackMsg := protocol.AcknowledgeMessage{
				SessionID: sessionID,
				Offset:    receivedSize,
				Status:    protocol.StatusOK,
			}

			ackBytes := protocol.EncodeAcknowledge(ackMsg)
			if err := protocol.SendMessage(conn, protocol.MsgTypeAcknowledge, ackBytes); err != nil {
				return "", fmt.Errorf("failed to send acknowledge message: %w", err)
			}
			log.Debugf("Sent acknowledge message, session ID: %s, offset: %d", sessionID, receivedSize)
		case protocol.MsgTypeFileComplete:
			completeMsg, err := protocol.DecodeFileComplete(bodyBytes)
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
			}

			endTime := time.Now()
			duration := endTime.Sub(startTime)
			throughput := float64(receivedSize) / duration.Seconds() / 1024 / 1024 // MB/s

			log.Infof("File download completed: %s, size: %d bytes, duration: %v, throughput: %.2f MB/s",
				filePath, receivedSize, duration, throughput)

			fileHash := completeMsg.FileHash
			return fmt.Sprintf("%x", fileHash), nil
		case protocol.MsgTypeError:
			errorMsg, err := protocol.DecodeErrorMessage(bodyBytes)
			if err != nil {
				return "", fmt.Errorf("error decoding error message: %w", err)
			}
			return "", fmt.Errorf("server error: %s", errorMsg.ErrorMessage)
		default:
			return "", fmt.Errorf("invalid file data message type, got %d", msgType)
		}
	}
}
