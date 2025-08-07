package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/internal/tree"
	"local-mirror/pkg/utils"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type fileServer struct {
	listenAddr string
	sessionMap sync.Map
}

type session struct {
	ID       [16]byte // 会话ID
	FilePath string   // 文件路径
	FileSize uint64   // 文件大小
	file     *os.File // 文件句柄
	fileHash [32]byte // 文件哈希值
}

func NewFileServer(listenAddr string) *fileServer {
	log.Info("Creating file server, listen address:", listenAddr)
	return &fileServer{
		listenAddr: listenAddr,
		sessionMap: sync.Map{},
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
	log.Infof("Client connected from %s to local port %s", clientAddr, conn.LocalAddr().String())

	for {
		msgType, bodyBytes, err := receiveMessage(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				log.Warnf("Client %s disconnected", clientAddr)
			} else {
				log.Error(fmt.Errorf("failed to receive message: %w", err))
			}
			return
		}

		switch msgType {
		case MsgTypeHandshake:
			if err := s.handleHandshake(conn, bodyBytes); err != nil {
				log.Error(err)
				return
			}
		case MsgTypeHeartbeatPing:
			if err := s.handlePingRequest(conn, bodyBytes); err != nil {
				log.Error("ping request:", err)
				errorMsg := ErrorMessage{
					MessageLen:   uint16(len(err.Error())),
					ErrorMessage: err.Error(),
				}
				errorBytes := encodeErrorMessage(errorMsg)
				if err := sendMessage(conn, MsgTypeError, StatusError, errorBytes); err != nil {
					log.Error("Error sending error message:", err)
				}
			}
		case MsgTypeTreeRequest:
			if err := s.handleTreeRequest(conn, bodyBytes); err != nil {
				log.Error("request:", err)
				errorMsg := ErrorMessage{
					MessageLen:   uint16(len(err.Error())),
					ErrorMessage: err.Error(),
				}
				errorBytes := encodeErrorMessage(errorMsg)
				if err := sendMessage(conn, MsgTypeError, StatusError, errorBytes); err != nil {
					log.Error("Error sending error message:", err)
				}
			}
		case MsgTypeFileRequest:
			if err := s.handleFileRequest(conn, bodyBytes); err != nil {
				log.Error("file request:", err)
				errorMsg := ErrorMessage{
					MessageLen:   uint16(len(err.Error())),
					ErrorMessage: err.Error(),
				}
				errorBytes := encodeErrorMessage(errorMsg)
				if err := sendMessage(conn, MsgTypeError, StatusError, errorBytes); err != nil {
					log.Error("Error sending errorm essage:", err)
				}
			}
		case MsgTypeAcknowledge:
			ackMsg, err := decodeAcknowledge(bodyBytes)
			if err != nil {
				log.Error("acknowledge message:", err)
				//todo: 给客户端发送错误消息
				return
			}
			log.Infof("Received acknowledge message: session ID: %s, offset: %d", ackMsg.SessionID, ackMsg.Offset)
		case MsgTypeFileComplete:
			completeMsg, err := decodeFileComplete(bodyBytes)
			if err != nil {
				log.Error("Error decoding file complete message:", err)
				//todo: 给客户端发送错误消息
				return
			}
			log.Infof("Received file complete message: session ID: %s", completeMsg.SessionID)
			s.sessionMap.Delete(completeMsg.SessionID)
		default:
			log.Errorf("Unknown message type: %d", msgType)
		}

	}
}

func (s *fileServer) handlePingRequest(conn net.Conn, bodyBytes []byte) error {
	pingRequest, err := decodeHeartbeatPing(bodyBytes)
	if err != nil {
		log.Error("Error decoding ping request message:", err)
		return err
	}
	log.Infof("Received ping request from %s, client ID: %d", conn.RemoteAddr().String(), pingRequest.ClientID)
	pongMessage := HeartbeatPongMessage{
		Version:   config.ProtocolVersion,
		Timestamp: time.Now().Unix(),
		ServerID:  config.InstanceID,
	}
	pongBytes := encodeHeartbeatPong(pongMessage)
	if err := sendMessage(conn, MsgTypeHeartbeatPong, StatusOK, pongBytes); err != nil {
		log.Error("Error sending pong message:", err)
	}
	log.Infof("Sent pong response to %s, server ID: %d", conn.RemoteAddr().String(), config.InstanceID)
	return nil
}

func (s *fileServer) handleTreeRequest(conn net.Conn, bodyBytes []byte) error {
	treeRequest, err := decodeTreeRequest(bodyBytes)
	if err != nil {
		return fmt.Errorf("error decoding tree request: %v", err)
	}
	clientAddr := conn.RemoteAddr().String()
	log.Infof("Received tree request from %s for path: %s", clientAddr, treeRequest.RootPath)
	treeLeaf, err := tree.GetDirContents(treeRequest.RootPath)
	if err != nil {
		return fmt.Errorf("error getting tree contents for path %s: %v", treeRequest.RootPath, err)
	} else {
		treeData, err := json.Marshal(treeLeaf)
		if err != nil {
			return fmt.Errorf("error marshalling tree leaf for path %s: %v", treeRequest.RootPath, err)
		}
		treeResponse := TreeResponseMessage{
			RootPath:   treeRequest.RootPath,
			DataLength: uint32(len(treeData)),
			Data:       []byte(treeData),
		}
		responseBytes := encodeTreeResponse(treeResponse)
		if err := sendMessage(conn, MsgTypeTreeResponse, StatusOK, responseBytes); err != nil {
			return fmt.Errorf("error sending tree response for path %s: %v", treeRequest.RootPath, err)
		}
		log.Infof("Sent tree response to %s for path: %s, data length: %d bytes", clientAddr, treeRequest.RootPath, len(treeData))
		return nil
	}
}

func (s *fileServer) handleFileRequest(conn net.Conn, bodyBytes []byte) error {
	fileRequest, err := decodeFileRequest(bodyBytes)
	if err != nil {
		return fmt.Errorf("error decoding file request: %v", err)
	}
	log.Debugf("Received file request: %s, offset: %d", fileRequest.FilePath, fileRequest.Offset)
	fullPath := filepath.Join(config.StartPath, fileRequest.FilePath)
	fileInfo, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file not found: %s", fileRequest.FilePath)
		}
		return fmt.Errorf("Error getting file info: %s :%v", fileRequest.FilePath, err)

	} else {
		fileHash, err := utils.CalcBlake3(fullPath)
		if err != nil {
			return fmt.Errorf("error calculating file hash for %s", fileRequest.FilePath)
		}

		file, err := os.Open(fullPath)
		if err != nil {
			return fmt.Errorf("error opening file %s", fileRequest.FilePath)
		}
		defer file.Close()

		sessionID, err := utils.RandomString(16)
		if err != nil {
			return fmt.Errorf("error generating session ID for file %s", fileRequest.FilePath)
		}
		var sessionBytes [16]byte
		copy(sessionBytes[:], sessionID)

		if fileRequest.Offset > 0 {
			if _, err := file.Seek(int64(fileRequest.Offset), io.SeekStart); err != nil {
				return fmt.Errorf("error seeking file %s at offset %d", fileRequest.FilePath, fileRequest.Offset)
			}
		}
		session := &session{
			ID:       sessionBytes,
			FilePath: fullPath,
			FileSize: uint64(fileInfo.Size()),
			file:     file,
			fileHash: fileHash,
		}

		s.sessionMap.Store(sessionID, session)

		fileResponse := FileResponseMessage{
			SessionID: sessionBytes,
			FileSize:  uint64(fileInfo.Size()),
			FileHash:  fileHash,
		}
		responseBytes := encodeFileResponse(fileResponse)
		if err := sendMessage(conn, MsgTypeFileResponse, StatusOK, responseBytes); err != nil {
			s.sessionMap.Delete(sessionID)
			return fmt.Errorf("error sending file response for %s", fileRequest.FilePath)
		}
		log.Debugf("Sent file response: session ID: %s, file size: %d bytes", sessionID, fileInfo.Size())
		if err := s.sendFileData(conn, session, fileRequest.Offset); err != nil {
			return err
		}
		return nil
	}
}

func (s *fileServer) sendFileData(conn net.Conn, session *session, offset uint64) error {
	defer session.file.Close()
	defer s.sessionMap.Delete(session.ID)

	fileBuf := make([]byte, *config.FileBufferSize)
	for {
		n, err := session.file.Read(fileBuf)
		if n > 0 {
			dataMsg := FileDataMessage{
				SessionID:  session.ID,
				Offset:     offset,
				DataLength: uint32(n),
				Data:       fileBuf[:n],
			}
			//todo: 添加发送失败重试发送的机制
			if err := sendMessage(conn, MsgTypeFileData, StatusOK, encodeFileData(dataMsg)); err != nil {
				return fmt.Errorf("error sending file data for %s", strings.Replace(session.FilePath, config.StartPath, ".", 1))
			}

			offset += uint64(n)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("error reading file %s", strings.Replace(session.FilePath, config.StartPath, ".", 1))
		}
	}
	completeMsg := FileCompleteMessage{
		SessionID: session.ID,
		FileHash:  session.fileHash,
	}

	completeBytes := encodeFileComplete(completeMsg)
	if err := sendMessage(conn, MsgTypeFileComplete, StatusOK, completeBytes); err != nil {
		return fmt.Errorf("error sending file complete for %s", strings.Replace(session.FilePath, config.StartPath, ".", 1))
	}
	log.Infof("Sent file complete message: file path: %s", strings.Replace(session.FilePath, config.StartPath, ".", 1))
	return nil
}

func (s *fileServer) handleHandshake(conn net.Conn, bodyBytes []byte) error {
	handshakeMsg, err := decodeHandshake(bodyBytes)
	if err != nil {
		return fmt.Errorf("failed to decode handshake: %w", err)
	}
	log.Infof("Received handshake message: version: %d, clientID: %d",
		handshakeMsg.Version,
		handshakeMsg.UUID)
	receiveHandshake := HandshakeMessage{
		Version: config.ProtocolVersion,
		UUID:    config.InstanceID,
		Role:    config.ModeMap[*config.Mode],
	}
	handshakeBytes := encodeHandshake(receiveHandshake)
	if err := sendMessage(conn, MsgTypeHandshake, StatusOK, handshakeBytes); err != nil {
		return fmt.Errorf("error sending handshake message: %v", err)
	}
	return nil
}
