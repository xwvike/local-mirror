package app

import (
	"encoding/json"
	"fmt"
	"io"
	"local-mirror/app/tree"
	"local-mirror/common/utils"
	"local-mirror/config"
	"net"
	"os"
	"path/filepath"
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
	log.Infof("Client connected: %s", clientAddr)

	if err := s.handleHandshake(conn); err != nil {
		log.Error("Error during handshake:", err)
		return
	}

	for {
		msgType, bodyBytes, err := receiveMessage(conn)
		if err != nil {
			if err != io.EOF {
				log.Errorf("Error receiving message from %s, %v\n", clientAddr, err)
			} else {
				log.Infof("Client %s disconnected", clientAddr)
			}
			break
		}

		switch msgType {
		case MsgTypeHandshake:
			if err := s.handlePingRequest(conn, bodyBytes); err != nil {
				log.Error("Error handling ping request:", err)
				errorMsg := ErrorMessage{
					ErrorCode:    StatusInternalError,
					MessageLen:   uint16(len(err.Error())),
					ErrorMessage: err.Error(),
				}
				errorBytes := encodeErrorMessage(errorMsg)
				if err := sendMessage(conn, MsgTypeError, errorBytes); err != nil {
					log.Error("Error sending error message:", err)
				}
			}
		case MsgTypeTreeRequest:
			if err := s.handleTreeRequest(conn, bodyBytes); err != nil {
				log.Error("Error handling tree request:", err)
				errorMsg := ErrorMessage{
					ErrorCode:    StatusInternalError,
					MessageLen:   uint16(len(err.Error())),
					ErrorMessage: err.Error(),
				}
				errorBytes := encodeErrorMessage(errorMsg)
				if err := sendMessage(conn, MsgTypeError, errorBytes); err != nil {
					log.Error("Error sending error message:", err)
				}
			}
		case MsgTypeFileRequest:
			if err := s.handleFileRequest(conn, bodyBytes); err != nil {
				log.Error("Error handling file request:", err)
				errorMsg := ErrorMessage{
					ErrorCode:    StatusInternalError,
					MessageLen:   uint16(len(err.Error())),
					ErrorMessage: err.Error(),
				}
				errorBytes := encodeErrorMessage(errorMsg)
				if err := sendMessage(conn, MsgTypeError, errorBytes); err != nil {
					log.Error("Error sending error message:", err)
				}
			}
		case MsgTypeAcknowledge:
			ackMsg, err := decodeAcknowledge(bodyBytes)
			if err != nil {
				log.Error("Error decoding acknowledge message:", err)
				return
			}
			log.Infof("Received acknowledge message: session ID: %s, offset: %d", ackMsg.SessionID, ackMsg.Offset)
		case MsgTypeFileComplete:
			completeMsg, err := decodeFileComplete(bodyBytes)
			if err != nil {
				log.Error("Error decoding file complete message:", err)
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
		Version:   config.Version,
		Timestamp: time.Now().Unix(),
		ServerID:  config.InstanceID,
	}
	pongBytes := encodeHeartbeatPong(pongMessage)
	if err := sendMessage(conn, MsgTypeHeartbeatPong, pongBytes); err != nil {
		log.Error("Error sending pong message:", err)
	}
	log.Infof("Sent pong response to %s, server ID: %d", conn.RemoteAddr().String(), config.InstanceID)
	return nil
}

func (s *fileServer) handleTreeRequest(conn net.Conn, bodyBytes []byte) error {
	treeRequest, err := decodeTreeRequest(bodyBytes)
	if err != nil {
		log.Error("Error decoding tree request message:", err)
		return err
	}
	clientAddr := conn.RemoteAddr().String()
	log.Infof("Received tree request from %s for path: %s", clientAddr, treeRequest.RootPath)
	fullTreePath := filepath.Join(config.StartPath, treeRequest.RootPath)
	treeLeaf, err := tree.GetDirContents(treeRequest.RootPath)
	if err != nil {
		log.Errorf("Error getting tree contents for path %s: %v", fullTreePath, err)
		errorMsg := ErrorMessage{
			ErrorCode:    StatusTreeNotFound,
			MessageLen:   uint16(len(err.Error())),
			ErrorMessage: err.Error(),
		}
		errorBytes := encodeErrorMessage(errorMsg)
		if err := sendMessage(conn, MsgTypeError, errorBytes); err != nil {
			log.Error("Error sending error message:", err)
			return err
		}
		return fmt.Errorf("error getting tree contents for path %s: %v", fullTreePath, err)
	} else {
		if len(treeLeaf) == 0 {
			log.Errorf("Tree path not found: %s", fullTreePath)
			errorMsg := ErrorMessage{
				ErrorCode:    StatusTreeNotFound,
				MessageLen:   uint16(len("Path not found")),
				ErrorMessage: "Path not found",
			}
			errorBytes := encodeErrorMessage(errorMsg)
			if err := sendMessage(conn, MsgTypeError, errorBytes); err != nil {
				log.Error("Error sending error message:", err)
			}
			return fmt.Errorf("path not found: %s", fullTreePath)
		}
		treeData, err := json.Marshal(treeLeaf)
		if err != nil {
			log.Error("Error marshalling tree leaf to JSON:", err)
			return err
		}
		treeResponse := TreeResponseMessage{
			Status:     StatusOK,
			RootPath:   treeRequest.RootPath,
			DataLength: uint32(len(treeData)),
			Data:       []byte(treeData),
		}
		responseBytes := encodeTreeResponse(treeResponse)
		if err := sendMessage(conn, MsgTypeTreeResponse, responseBytes); err != nil {
			log.Error("Error sending tree response message:", err)
			return err
		}
		log.Infof("Sent tree response to %s for path: %s, data length: %d bytes", clientAddr, treeRequest.RootPath, len(treeData))
		return nil
	}
}

func (s *fileServer) handleFileRequest(conn net.Conn, bodyBytes []byte) error {
	fileRequest, err := decodeFileRequest(bodyBytes)
	if err != nil {
		log.Error("Error decoding file request message:", err)
		return err
	}
	log.Debugf("Received file request: %s, offset: %d", fileRequest.FilePath, fileRequest.Offset)
	fullPath := filepath.Join(config.StartPath, fileRequest.FilePath)
	fileInfo, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Errorf("File not found: %s", fullPath)
			errorMsg := ErrorMessage{
				ErrorCode:    StatusFileNotFound,
				MessageLen:   uint16(len(err.Error())),
				ErrorMessage: err.Error(),
			}
			errorBytes := encodeErrorMessage(errorMsg)
			if err := sendMessage(conn, MsgTypeError, errorBytes); err != nil {
				log.Error("Error sending error message:", err)
			}
		}
	}

	fileHash, err := utils.CalcBlake3(fullPath)
	if err != nil {
		log.Error("Error calculating file hash:", err)
		return err
	}

	file, err := os.Open(fullPath)
	if err != nil {
		log.Error("Error opening file:", err)
		return err
	}

	sessionID, err := utils.RandomString(16)
	var sessionBytes [16]byte
	copy(sessionBytes[:], sessionID)

	if fileRequest.Offset > 0 {
		if _, err := file.Seek(int64(fileRequest.Offset), io.SeekStart); err != nil {
			log.Error("Error seeking file:", err)
			file.Close()
			return err
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
		Status:    StatusOK,
		SessionID: sessionBytes,
		FileSize:  uint64(fileInfo.Size()),
		FileHash:  fileHash,
	}
	responseBytes := encodeFileResponse(fileResponse)
	if err := sendMessage(conn, MsgTypeFileResponse, responseBytes); err != nil {
		log.Error("Error sending file response message:", err)
		file.Close()
		s.sessionMap.Delete(sessionID)
		return err
	}
	log.Debugf("Sent file response: session ID: %s, file size: %d bytes", sessionID, fileInfo.Size())
	if err := s.sendFileData(conn, session, fileRequest.Offset); err != nil {
		log.Error("Error sending file data:", err)
	}
	return nil
}

func (s *fileServer) sendFileData(conn net.Conn, session *session, offset uint64) error {
	defer session.file.Close()
	defer s.sessionMap.Delete(session.ID)

	fileBuf := make([]byte, config.FileBufferSize)
	for {
		n, err := session.file.Read(fileBuf)
		if err != nil {
			if err != io.EOF {
				log.Error("Error reading file:", err)
				errMsg := ErrorMessage{
					ErrorCode:    StatusInternalError,
					MessageLen:   uint16(len(err.Error())),
					ErrorMessage: err.Error(),
				}
				errorBytes := encodeErrorMessage(errMsg)
				sendMessage(conn, MsgTypeError, errorBytes)
			}
			break
		}

		dataMsg := FileDataMessage{
			SessionID:  session.ID,
			Offset:     offset,
			DataLength: uint32(n),
			Data:       fileBuf[:n],
		}

		if err := sendMessage(conn, MsgTypeFileData, encodeFileData(dataMsg)); err != nil {
			log.Error("Error sending file data message:", err)
			return err
		}

		offset += uint64(n)
	}
	completeMsg := FileCompleteMessage{
		SessionID: session.ID,
		FileHash:  session.fileHash,
	}

	completeBytes := encodeFileComplete(completeMsg)
	if err := sendMessage(conn, MsgTypeFileComplete, completeBytes); err != nil {
		log.Error("Error sending file complete message:", err)
		return err
	}
	log.Infof("Sent file complete message: session ID: %s", session.ID)
	return nil
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
		handshakeMsg.UUID,
		handshakeMsg.Role)
	receiveHandshake := HandshakeMessage{
		Version: config.Version,
		UUID:    config.InstanceID,
		Role:    config.ModeMap[*config.Mode],
	}
	handshakeBytes := encodeHandshake(receiveHandshake)
	if err := sendMessage(conn, MsgTypeHandshake, handshakeBytes); err != nil {
		log.Error("Error sending handshake message:", err)
		return err
	}
	return nil
}
