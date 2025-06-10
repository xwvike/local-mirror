package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"local-mirror/common/utils"
	"local-mirror/config"
	"net"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
)

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
		Version: config.Version,
		UUID:    config.InstanceID,
		Role:    config.ModeMap[*config.Mode],
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
		handshakeResponse.UUID,
		handshakeResponse.Role)
	return conn, nil
}

func (c *fileClient) Ping(conn net.Conn) error {
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

func (c *fileClient) GetRealityTree(conn net.Conn, rootPath string) (map[string]interface{}, error) {
	request := TreeRequestMessage{
		PathLength: uint16(len(rootPath)),
		RootPath:   rootPath,
	}
	requestBytes := encodeTreeRequest(request)
	serverAddr := conn.RemoteAddr().String()
	if err := sendMessage(conn, MsgTypeTreeRequest, requestBytes); err != nil {
		log.Error("Error sending tree request message:", err)
		return nil, err
	}
	log.Infof("Sent tree request to %s for path: %s", serverAddr, rootPath)
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		log.Error("Error receiving tree response:", err)
		return nil, err
	}
	if msgType == MsgTypeError {
		errorMsg, err := decodeErrorMessage(bodyBytes)
		if err != nil {
			log.Error("Error decoding error message:", err)
			return nil, err
		}
		newError := fmt.Errorf("server error: %s", errorMsg.ErrorMessage)
		log.Error(newError)
		return nil, newError
	}
	if msgType != MsgTypeTreeResponse {
		newError := fmt.Errorf("invalid tree response message type, got %d", msgType)
		log.Error(newError)
		return nil, newError
	}
	treeResponse, err := decodeTreeResponse(bodyBytes)
	if err != nil {
		log.Error("Error decoding tree response:", err)
		return nil, err
	}
	log.Infof("Received tree response from %s, status: %d, data length: %d bytes",
		serverAddr,
		treeResponse.Status,
		len(treeResponse.Data))
	log.Debugf("Received tree response: %s", treeResponse.Data)

	if treeResponse.Status != StatusOK {
		newError := fmt.Errorf("tree request failed, status code: %d", treeResponse.Status)
		log.Error(newError)
		return nil, newError
	}
	var realityTree map[string]interface{}
	json.Unmarshal(treeResponse.Data, &realityTree)
	if realityTree == nil {
		newError := fmt.Errorf("tree response data is nil")
		log.Error(newError)
		return nil, newError
	}
	return realityTree, nil
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
	cacheFile := new(bytes.Buffer)

	if fileResponse.FileSize <= 0 {
		file.Close()
		log.Debug("create empty file:", fullPath)
		return nil
	} else if fileResponse.FileSize > 0 {
		for {
			msgType, bodyBytes, err := receiveMessage(conn)
			if err != nil {
				log.Error("Error receiving file data:", err)
				return err
			}
			switch msgType {
			case MsgTypeFileData:
				dataMsg, err := decodeFileData(bodyBytes)
				if err != nil {
					log.Error("Error decoding file data message:", err)
					return err
				}
				// todo: check session ID
				if dataMsg.SessionID != sessionID {
					log.Error("Invalid session ID in file data message")
					return fmt.Errorf("invalid session ID in file data message, got %s", dataMsg.SessionID)
				}

				if fileResponse.FileSize <= config.MemFileThreshold {
					cacheFile.Write(dataMsg.Data)
					receivedSize += uint64(len(dataMsg.Data))
				} else {
					if _, err := file.Write(dataMsg.Data); err != nil {
						log.Error("Error writing file data:", err)
						return err
					}
					receivedSize += uint64(len(dataMsg.Data))
					ackMsg := AcknowledgeMessage{
						SessionID: sessionID,
						Offset:    receivedSize,
						Status:    StatusOK,
					}

					ackBytes := encodeAcknowlege(ackMsg)
					if err := sendMessage(conn, MsgTypeAcknowledge, ackBytes); err != nil {
						log.Error("Error sending acknowledge message:", err)
						return err
					}
					log.Debugf("Sent acknowledge message, session ID: %s, offset: %d", sessionID, receivedSize)
				}
			case MsgTypeFileComplete:
				completeMsg, err := decodeFileComplete(bodyBytes)
				if err != nil {
					log.Error("Error decoding file complete message:", err)
					return err
				}
				if completeMsg.SessionID != sessionID {
					newError := fmt.Errorf("invalid session ID in file complete message, got %s", completeMsg.SessionID)
					log.Error(newError)
					return newError
				}
				if fileResponse.FileSize <= config.MemFileThreshold {
					if _, err := file.Write(cacheFile.Bytes()); err != nil {
						log.Error("Error writing cached file data:", err)
						return err
					}
					cacheFile.Reset()
				} else {
					if err := file.Close(); err != nil {
						log.Error("Error closing file:", err)
						return err
					}
				}

				file.Sync()
				fileHash, err := utils.CalcBlake3(fullPath)
				if err != nil {
					log.Error("Error calculating file hash:", err)
					return err
				}
				if fileHash != completeMsg.FileHash {
					newError := fmt.Errorf("file hash mismatch, expected %x, got %x", completeMsg.FileHash, fileHash)
					log.Error(newError)
					return newError
				}
				transferSpeed := float64(fileResponse.FileSize) / time.Since(startTime).Seconds()
				log.Infof("File transfer complete, session ID: %s, file size: %d bytes, transfer speed: %.2f MB/s",
					sessionID,
					fileResponse.FileSize,
					transferSpeed/1024/1024)
				return nil
			case MsgTypeError:
				errorMsg, err := decodeErrorMessage(bodyBytes)
				if err != nil {
					log.Error("Error decoding error message:", err)
					return err
				}
				newError := fmt.Errorf("server error: %s", errorMsg.ErrorMessage)
				log.Error(newError)
				return newError
			default:
				newError := fmt.Errorf("invalid file data message type, got %d", msgType)
				log.Error(newError)
				return newError
			}
		}
	}
	return nil
}
