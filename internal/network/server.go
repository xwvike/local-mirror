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
	clientMap  sync.Map
}

type session struct {
	ID       [16]byte // 会话ID
	FilePath string   // 文件路径
	FileSize uint64   // 文件大小
	file     *os.File // 文件句柄
	fileHash [32]byte // 文件哈希值
}

type client struct {
	ID             uint32    // 客户端ID
	Alias          string    // 客户端别名
	Addr           string    // 客户端地址
	Role           uint8     // 客户端角色
	LastActiveTime time.Time // 最后一次通讯时间
	Version        uint16    // 客户端协议版本
	Connected      bool      // 当前是否已连接
	Conn           net.Conn  // 客户端连接
	SessionIDs     []string  // 活跃的会话ID列表
}

func (c *client) AddSessionID(sessionID string) {
	c.SessionIDs = append(c.SessionIDs, sessionID)
}
func (c *client) RemoveSessionID(sessionID string) {
	for i, id := range c.SessionIDs {
		if id == sessionID {
			c.SessionIDs = append(c.SessionIDs[:i], c.SessionIDs[i+1:]...)
			return
		}
	}
}
func (c *client) UpdateLastActiveTime() {
	c.LastActiveTime = time.Now()
}

func (s *fileServer) GetAllClients() []*client {
	clients := make([]*client, 0)
	s.clientMap.Range(func(key, value interface{}) bool {
		if c, ok := value.(*client); ok {
			clients = append(clients, c)
		}
		return true
	})
	return clients
}

func NewFileServer(listenAddr string) *fileServer {
	log.Info("Creating file server, listen address:", listenAddr)
	return &fileServer{
		listenAddr: listenAddr,
		sessionMap: sync.Map{},
		clientMap:  sync.Map{},
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
	clientAddr := conn.RemoteAddr().String()
	log.Infof("Client connected from %s to local port %s", clientAddr, conn.LocalAddr().String())
	client := &client{
		ID:             0,
		Alias:          "",
		Addr:           clientAddr,
		Role:           0,
		LastActiveTime: time.Now(),
		Version:        0,
		Connected:      false,
		Conn:           conn,
		SessionIDs:     []string{},
	}

	defer func() {
		if err := conn.Close(); err != nil {
			log.Errorf("Error closing connection for %s: %v", clientAddr, err)
		}
		s.clientMap.Delete(client.ID)
	}()

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
		client.UpdateLastActiveTime()

		switch msgType {
		case MsgTypeHandshake:
			clientBase, err := s.handleHandshake(conn, bodyBytes)
			if err != nil {
				conn.Close()
				log.Error(err)
				return
			}
			client.ID = clientBase.UUID
			client.Alias = ""
			client.Role = clientBase.Role
			client.Version = clientBase.Version
			client.Connected = true
			s.clientMap.Store(clientBase.UUID, client)
		case MsgTypeReverify:
			if err := s.handleReverify(client.ID); err != nil {
				log.Error(err)
				return
			}
		case MsgTypeHeartbeatPing:
			if err := s.handlePingRequest(client.ID, bodyBytes); err != nil {
				log.Error(err)
				if errors.Is(err, appError.ErrConnection) {
					conn.Close()
					s.clientMap.Delete(client.ID)
					log.Warnf("Connection closed for %s due to error: %v", clientAddr, err)
					return
				} else {
					errorMsg := ErrorMessage{
						MessageLen:   uint16(len(err.Error())),
						ErrorMessage: err.Error(),
					}
					errorBytes := encodeErrorMessage(errorMsg)
					sendMessage(conn, MsgTypeError, StatusError, errorBytes)
				}

			}
		case MsgTypeTreeRequest:
			if err := s.handleTreeRequest(client.ID, bodyBytes); err != nil {
				log.Error(err)
				if errors.Is(err, appError.ErrConnection) {
					conn.Close()
					s.clientMap.Delete(client.ID)
					log.Warnf("Connection closed for %s due to error: %v", clientAddr, err)
					return
				} else {
					errorMsg := ErrorMessage{
						MessageLen:   uint16(len(err.Error())),
						ErrorMessage: err.Error(),
					}
					errorBytes := encodeErrorMessage(errorMsg)
					sendMessage(conn, MsgTypeError, StatusError, errorBytes)
				}
			}
		case MsgTypeFileRequest:
			if err := s.handleFileRequest(client.ID, bodyBytes); err != nil {
				log.Error(err)
				if errors.Is(err, appError.ErrConnection) {
					conn.Close()
					s.clientMap.Delete(client.ID)
					log.Warnf("Connection closed for %s due to error: %v", clientAddr, err)
					return
				} else {
					errorMsg := ErrorMessage{
						MessageLen:   uint16(len(err.Error())),
						ErrorMessage: err.Error(),
					}
					errorBytes := encodeErrorMessage(errorMsg)
					sendMessage(conn, MsgTypeError, StatusError, errorBytes)
				}
			}
		case MsgTypeAcknowledge:
			ackMsg, err := decodeAcknowledge(bodyBytes)
			if err != nil {
				conn.Close()
				s.clientMap.Delete(client.ID)
				log.Warn("acknowledge message:", err)
				//todo: 给客户端发送回复
				return
			}
			log.Infof("Received acknowledge message: session ID: %s, offset: %d", ackMsg.SessionID, ackMsg.Offset)
		case MsgTypeFileComplete:
			completeMsg, err := decodeFileComplete(bodyBytes)
			if err != nil {
				conn.Close()
				s.clientMap.Delete(client.ID)
				log.Warn("Error decoding file complete message:", err)
				//todo: 给客户端发送回复
				return
			}
			log.Infof("Received file complete message: session ID: %s", completeMsg.SessionID)
			s.sessionMap.Delete(completeMsg.SessionID)
		default:
			log.Errorf("Unknown message type: %d", msgType)
		}

	}
}

func (s *fileServer) handlePingRequest(ID uint32, bodyBytes []byte) error {
	_client, ok := s.clientMap.Load(ID)
	if !ok {
		return fmt.Errorf("%w, client not found for ID: %d", appError.ErrConnection, ID)
	}
	conn := _client.(*client).Conn
	pingRequest, err := decodeHeartbeatPing(bodyBytes)
	if err != nil {
		log.Error("%w, Error decoding ping request message:", appError.ErrConnection, err)
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
		log.Error("%w, Error sending pong message:", appError.ErrConnection, err)
	}
	log.Infof("Sent pong response to %s, server ID: %d", conn.RemoteAddr().String(), config.InstanceID)
	return nil
}

func (s *fileServer) handleTreeRequest(ID uint32, bodyBytes []byte) error {
	_client, ok := s.clientMap.Load(ID)
	if !ok {
		return fmt.Errorf("%w, client not found for ID: %d", appError.ErrConnection, ID)
	}
	conn := _client.(*client).Conn
	treeRequest, err := decodeTreeRequest(bodyBytes)
	if err != nil {
		return fmt.Errorf("%w, error decoding tree request: %v", appError.ErrConnection, err)
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
			return fmt.Errorf("%w, error sending tree response for path %s: %v", appError.ErrConnection, treeRequest.RootPath, err)
		}
		log.Infof("Sent tree response to %s for path: %s, data length: %d bytes", clientAddr, treeRequest.RootPath, len(treeData))
		return nil
	}
}

func (s *fileServer) handleFileRequest(ID uint32, bodyBytes []byte) error {
	_client, ok := s.clientMap.Load(ID)
	if !ok {
		return fmt.Errorf("%w, client not found for ID: %d", appError.ErrConnection, ID)
	}
	conn := _client.(*client).Conn
	fileRequest, err := decodeFileRequest(bodyBytes)
	if err != nil {
		return fmt.Errorf("%w, error decoding file request: %v", appError.ErrConnection, err)
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
			return fmt.Errorf("%w, error sending file response for %s", appError.ErrConnection, fileRequest.FilePath)
		}
		log.Debugf("Sent file response: session ID: %s, file size: %d bytes", sessionID, fileInfo.Size())
		if err := s.sendFileData(ID, session, fileRequest.Offset); err != nil {
			return err
		}
		return nil
	}
}

func (s *fileServer) sendFileData(ID uint32, session *session, offset uint64) error {
	_client, ok := s.clientMap.Load(ID)
	if !ok {
		return fmt.Errorf("%w, client not found for ID: %d", appError.ErrConnection, ID)
	}
	conn := _client.(*client).Conn
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
			if err := sendMessage(conn, MsgTypeFileData, StatusOK, encodeFileData(dataMsg)); err != nil {
				return fmt.Errorf("%w, error sending file data for %s", appError.ErrConnection, strings.Replace(session.FilePath, config.StartPath, ".", 1))
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
		return fmt.Errorf("%w, error sending file complete for %s", appError.ErrConnection, strings.Replace(session.FilePath, config.StartPath, ".", 1))
	}
	log.Infof("Sent file complete message: file path: %s", strings.Replace(session.FilePath, config.StartPath, ".", 1))
	return nil
}

func (s *fileServer) handleHandshake(conn net.Conn, bodyBytes []byte) (*HandshakeMessage, error) {
	handshakeMsg, err := decodeHandshake(bodyBytes)
	if err != nil {
		return nil, fmt.Errorf("%w, failed to decode handshake: %w", appError.ErrConnection, err)
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
		return nil, fmt.Errorf("%w, error sending handshake message: %v", appError.ErrConnection, err)
	}
	return &handshakeMsg, nil
}

func (s *fileServer) handleReverify(ID uint32) error {
	_client, ok := s.clientMap.Load(ID)
	if !ok {
		return fmt.Errorf("%w, client not found for ID: %d", appError.ErrConnection, ID)
	}
	conn := _client.(*client).Conn
	reverifyResponse := ReverifyResponse{
		Version:  config.ProtocolVersion,
		ServerID: config.InstanceID,
	}
	responseBytes := encodeReverifyResponse(reverifyResponse)
	if err := sendMessage(conn, MsgTypeReverifyResponse, StatusOK, responseBytes); err != nil {
		return fmt.Errorf("%w, error sending reverify response: %v", appError.ErrConnection, err)
	}
	return nil
}
