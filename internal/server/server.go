package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"local-mirror/config"
	"local-mirror/internal/protocol"
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

type FileServer struct {
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

func NewFileServer(listenAddr string) *FileServer {
	log.Info("Creating file server, listen address:", listenAddr)
	return &FileServer{
		listenAddr: listenAddr,
		sessionMap: sync.Map{},
	}
}

func (s *FileServer) Start() error {
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

func (s *FileServer) handleConnection(conn net.Conn) {
	defer conn.Close()

	clientAddr := conn.RemoteAddr().String()
	log.Infof("Client connected from %s to local port %s", clientAddr, conn.LocalAddr().String())

	if err := s.handleHandshake(conn); err != nil {
		log.Error("Error during handshake:", err)
		return
	}

	for {
		msgType, bodyBytes, err := protocol.ReceiveMessage(conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				log.Infof("Client %s disconnected", clientAddr)
			} else {
				log.Errorf("Error receiving message from %s, %v\n", clientAddr, err)
			}
			break
		}

		switch msgType {
		case protocol.MsgTypeHeartbeatPing:
			if err := s.handlePingRequest(conn, bodyBytes); err != nil {
				log.Error("ping request:", err)
				errorMsg := protocol.ErrorMessage{
					ErrorCode:    protocol.StatusInternalError,
					MessageLen:   uint16(len(err.Error())),
					ErrorMessage: err.Error(),
				}
				errorBytes := protocol.EncodeErrorMessage(errorMsg)
				if err := protocol.SendMessage(conn, protocol.MsgTypeError, errorBytes); err != nil {
					log.Error("Error sending error message:", err)
				}
			}
		case protocol.MsgTypeTreeRequest:
			if err := s.handleTreeRequest(conn, bodyBytes); err != nil {
				log.Error("request:", err)
				errorMsg := protocol.ErrorMessage{
					ErrorCode:    protocol.StatusInternalError,
					MessageLen:   uint16(len(err.Error())),
					ErrorMessage: err.Error(),
				}
				errorBytes := protocol.EncodeErrorMessage(errorMsg)
				if err := protocol.SendMessage(conn, protocol.MsgTypeError, errorBytes); err != nil {
					log.Error("Error sending error message:", err)
				}
			}
		case protocol.MsgTypeFileRequest:
			if err := s.handleFileRequest(conn, bodyBytes); err != nil {
				log.Error("file request:", err)
				errorMsg := protocol.ErrorMessage{
					ErrorCode:    protocol.StatusInternalError,
					MessageLen:   uint16(len(err.Error())),
					ErrorMessage: err.Error(),
				}
				errorBytes := protocol.EncodeErrorMessage(errorMsg)
				if err := protocol.SendMessage(conn, protocol.MsgTypeError, errorBytes); err != nil {
					log.Error("Error sending error message:", err)
				}
			}
		case protocol.MsgTypeAcknowledge:
			ackMsg, err := protocol.DecodeAcknowledge(bodyBytes)
			if err != nil {
				log.Error("acknowledge request:", err)
				continue
			}
			sessionInterface, exists := s.sessionMap.Load(ackMsg.SessionID)
			if !exists {
				log.Errorf("Session not found: %s", ackMsg.SessionID)
				continue
			}
			session := sessionInterface.(*session)
			if err := s.sendFileData(conn, session, ackMsg.Offset); err != nil {
				log.Error("file data sending:", err)
			}
		case protocol.MsgTypeFileComplete:
			completeMsg, err := protocol.DecodeFileComplete(bodyBytes)
			if err != nil {
				log.Error("file complete request:", err)
				continue
			}
			log.Infof("File transfer complete: %s", completeMsg.SessionID)
			s.sessionMap.Delete(completeMsg.SessionID)
		default:
			log.Errorf("Unknown message type: %d", msgType)
		}
	}
}

func (s *FileServer) handleTreeRequest(conn net.Conn, bodyBytes []byte) error {
	treeRequest, err := protocol.DecodeTreeRequest(bodyBytes)
	if err != nil {
		return fmt.Errorf("error decoding tree request: %v", err)
	}
	clientAddr := conn.RemoteAddr().String()
	log.Infof("Received tree request from %s for path: %s", clientAddr, treeRequest.RootPath)
	treeLeaf, err := tree.GetDirectoryContents(treeRequest.RootPath)
	if err != nil {
		return fmt.Errorf("error getting tree contents for path %s: %v", treeRequest.RootPath, err)
	} else {
		treeData, err := json.Marshal(treeLeaf)
		if err != nil {
			return fmt.Errorf("error marshalling tree leaf for path %s: %v", treeRequest.RootPath, err)
		}
		treeResponse := protocol.TreeResponseMessage{
			Status:     protocol.StatusOK,
			RootPath:   treeRequest.RootPath,
			DataLength: uint32(len(treeData)),
			Data:       []byte(treeData),
		}
		treeResponseBytes := protocol.EncodeTreeResponse(treeResponse)
		if err := protocol.SendMessage(conn, protocol.MsgTypeTreeResponse, treeResponseBytes); err != nil {
			return fmt.Errorf("error sending tree response: %v", err)
		}
		log.Infof("Sent tree response to %s for path: %s, data length: %d bytes",
			clientAddr,
			treeRequest.RootPath,
			len(treeData))
		return nil
	}
}

func (s *FileServer) handleFileRequest(conn net.Conn, bodyBytes []byte) error {
	fileRequest, err := protocol.DecodeFileRequest(bodyBytes)
	if err != nil {
		return fmt.Errorf("error decoding file request: %v", err)
	}
	clientAddr := conn.RemoteAddr().String()
	log.Infof("Received file request from %s for file: %s", clientAddr, fileRequest.FilePath)

	// 检查文件路径安全性
	cleanPath := filepath.Clean(fileRequest.FilePath)
	if strings.Contains(cleanPath, "..") {
		return fmt.Errorf("invalid file path: %s", fileRequest.FilePath)
	}

	fullPath := filepath.Join(config.StartPath, cleanPath)
	fileInfo, err := os.Stat(fullPath)
	if err != nil {
		log.Errorf("File not found: %s", fullPath)
		return fmt.Errorf("file not found: %s", fileRequest.FilePath)
	}

	if fileInfo.IsDir() {
		return fmt.Errorf("requested path is a directory: %s", fileRequest.FilePath)
	}

	file, err := os.Open(fullPath)
	if err != nil {
		return fmt.Errorf("error opening file %s: %v", fullPath, err)
	}

	// 生成会话ID
	sessionIDStr, _ := utils.GenerateRandomString(16)
	var sessionID [16]byte
	copy(sessionID[:], sessionIDStr)

	// 计算文件哈希
	fileHash, err := utils.CalculateBlake3Hash(fullPath)
	if err != nil {
		file.Close()
		return fmt.Errorf("error calculating file hash for %s: %v", fullPath, err)
	}

	// 创建会话
	newSession := &session{
		ID:       sessionID,
		FilePath: fileRequest.FilePath,
		FileSize: uint64(fileInfo.Size()),
		file:     file,
		fileHash: fileHash,
	}

	s.sessionMap.Store(sessionID, newSession)

	// 发送文件响应
	fileResponse := protocol.FileResponseMessage{
		Status:    protocol.StatusOK,
		SessionID: sessionID,
		FileSize:  uint64(fileInfo.Size()),
		FileHash:  fileHash,
	}

	responseBytes := protocol.EncodeFileResponse(fileResponse)
	if err := protocol.SendMessage(conn, protocol.MsgTypeFileResponse, responseBytes); err != nil {
		file.Close()
		s.sessionMap.Delete(sessionID)
		return fmt.Errorf("error sending file response: %v", err)
	}

	log.Infof("Created session %s for file: %s, size: %d bytes",
		sessionID, fileRequest.FilePath, fileInfo.Size())
	return nil
}

func (s *FileServer) sendFileData(conn net.Conn, session *session, offset uint64) error {
	// 移动文件指针到指定偏移
	if _, err := session.file.Seek(int64(offset), io.SeekStart); err != nil {
		return fmt.Errorf("error seeking file: %v", err)
	}

	buffer := make([]byte, *config.FileBufferSize)
	n, err := session.file.Read(buffer)
	if err != nil && err != io.EOF {
		return fmt.Errorf("error reading file: %v", err)
	}

	if n > 0 {
		fileData := protocol.FileDataMessage{
			SessionID:  session.ID,
			Offset:     offset,
			DataLength: uint32(n),
			Data:       buffer[:n],
		}

		dataBytes := protocol.EncodeFileData(fileData)
		if err := protocol.SendMessage(conn, protocol.MsgTypeFileData, dataBytes); err != nil {
			return fmt.Errorf("error sending file data: %v", err)
		}

		log.Debugf("Sent %d bytes for session %s, offset: %d", n, session.ID, offset)
	}

	// 检查是否传输完成
	if offset+uint64(n) >= session.FileSize {
		completeMsg := protocol.FileCompleteMessage{
			SessionID: session.ID,
			FileHash:  session.fileHash,
		}

		completeBytes := protocol.EncodeFileComplete(completeMsg)
		if err := protocol.SendMessage(conn, protocol.MsgTypeFileComplete, completeBytes); err != nil {
			return fmt.Errorf("error sending file complete: %v", err)
		}

		log.Infof("File transfer completed for session %s", session.ID)
		session.file.Close()
	}

	return nil
}

func (s *FileServer) handlePingRequest(conn net.Conn, bodyBytes []byte) error {
	pingMsg, err := protocol.DecodeHeartbeatPing(bodyBytes)
	if err != nil {
		return fmt.Errorf("error decoding ping message: %v", err)
	}

	pongMsg := protocol.HeartbeatPongMessage{
		Version:   config.Version,
		Timestamp: time.Now().Unix(),
		ServerID:  config.InstanceID,
	}

	pongBytes := protocol.EncodeHeartbeatPong(pongMsg)
	if err := protocol.SendMessage(conn, protocol.MsgTypeHeartbeatPong, pongBytes); err != nil {
		return fmt.Errorf("error sending pong message: %v", err)
	}

	log.Debugf("Responded to ping from client %d", pingMsg.ClientID)
	return nil
}

func (s *FileServer) handleHandshake(conn net.Conn) error {
	msgType, bodyBytes, err := protocol.ReceiveMessage(conn)
	if err != nil {
		return fmt.Errorf("error receiving message: %v", err)
	}
	if msgType != protocol.MsgTypeHandshake {
		return fmt.Errorf("invalid message type, got message type %d", msgType)
	}

	handshakeMsg, err := protocol.DecodeHandshake(bodyBytes)
	if err != nil {
		return fmt.Errorf("error decoding handshake message: %v", err)
	}
	log.Infof("Received handshake message: version: %d, clientID: %d",
		handshakeMsg.Version,
		handshakeMsg.UUID)
	receiveHandshake := protocol.HandshakeMessage{
		Version: config.Version,
		UUID:    config.InstanceID,
		Role:    config.ModeMap[*config.Mode],
	}
	handshakeBytes := protocol.EncodeHandshake(receiveHandshake)
	if err := protocol.SendMessage(conn, protocol.MsgTypeHandshake, handshakeBytes); err != nil {
		return fmt.Errorf("error sending handshake message: %v", err)
	}
	return nil
}
