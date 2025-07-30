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

	handshakeMsg := HandshakeMessage{
		Version: config.Version,
		UUID:    config.InstanceID,
		Role:    config.ModeMap[*config.Mode],
	}
	handshakeBytes := encodeHandshake(handshakeMsg)

	if err := sendMessage(conn, MsgTypeHandshake, handshakeBytes); err != nil {
		return nil, fmt.Errorf("failed to send handshake message: %w", err)
	}
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to receive handshake response: %w", err)
	}

	if msgType != MsgTypeHandshake {
		conn.Close()
		return nil, fmt.Errorf("invalid handshake response message type, got %d", msgType)
	}
	handshakeResponse, err := decodeHandshake(bodyBytes)
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
	pingMessage := HeartbeatPingMessage{
		Version:   config.Version,
		Timestamp: time.Now().Unix(),
		ClientID:  config.InstanceID,
	}
	pingBytes := encodeHeartbeatPing(pingMessage)
	if err := sendMessage(conn, MsgTypeHeartbeatPing, pingBytes); err != nil {
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

func (c *FileClient) GetRealityTree(conn net.Conn, rootPath string) ([]byte, error) {
	request := TreeRequestMessage{
		PathLength: uint16(len(rootPath)),
		RootPath:   rootPath,
	}
	requestBytes := encodeTreeRequest(request)
	serverAddr := conn.RemoteAddr().String()
	if err := sendMessage(conn, MsgTypeTreeRequest, requestBytes); err != nil {
		return nil, fmt.Errorf("failed to send tree request: %w", err)
	}
	log.Debugf("Sent tree request to %s for path: %s", serverAddr, rootPath)
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

	if treeResponse.Status != StatusOK {
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
	requestFile := FileRequestMessage{
		PathLength: uint16(len(filePath)),
		FilePath:   filePath,
		Offset:     0,
	}
	requestBytes := encodeFileRequest(requestFile)
	if err := sendMessage(conn, MsgTypeFileRequest, requestBytes); err != nil {
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
				Status:    StatusOK,
			}

			ackBytes := encodeAcknowlege(ackMsg)
			if err := sendMessage(conn, MsgTypeAcknowledge, ackBytes); err != nil {
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
			return "", fmt.Errorf("server error: %w", errorMsg)
		default:
			return "", fmt.Errorf("invalid file data message type, got %d", msgType)
		}
	}
}
