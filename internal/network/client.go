package network

import (
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/pkg/utils"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// ConnectionState 描述客户端连接的生命周期状态。
// 使用自定义类型而非 uint8，让编译器在类型赋值时提供保护。
type ConnectionState uint8

const (
	Waiting    ConnectionState = iota // 0x00 已建立TCP连接，等待握手
	Online                            // 0x01 握手成功，可以正常通信
	Offline                           // 0x02 连接已断开
	Deprecated                        // 0x03 连接不可恢复，需要丢弃
)

type ConnectionManager struct {
	conn        net.Conn
	mutex       sync.RWMutex
	connectAddr string
	maxRetries  int
	retryDelay  time.Duration
}

func NewConnectionManager(addr string) (*ConnectionManager, error) {
	// 带超时拨号：端口扫描时不能在无响应的地址上无限期等待
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", addr, err)
	}
	return &ConnectionManager{
		connectAddr: addr,
		maxRetries:  3,
		retryDelay:  3 * time.Second,
		conn:        conn,
	}, nil
}

func (cm *ConnectionManager) GetConnection() (net.Conn, error) {
	// defer 统一放在函数入口，无论哪条返回路径都能正确释放读锁
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	if cm.conn != nil && cm.isConnValid() {
		return cm.conn, nil
	}
	return nil, fmt.Errorf("connection is invalid")
}

// todo: 需要添加使用心跳检测连接是否有效
func (cm *ConnectionManager) isConnValid() bool {
	return true
}

func (cm *ConnectionManager) Reconnect() error {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	if cm.conn != nil {
		cm.conn.Close()
		cm.conn = nil
	}

	var err error
	for i := 0; i < cm.maxRetries; i++ {
		log.Infof("Attempting to reconnect (attempt %d/%d)", i+1, cm.maxRetries)

		cm.conn, err = net.DialTimeout("tcp", cm.connectAddr, 3*time.Second)
		if err == nil {
			log.Info("Reconnection successful")
			return nil
		}

		log.Errorf("Reconnection attempt %d failed: %v", i+1, err)
		if i < cm.maxRetries-1 {
			time.Sleep(cm.retryDelay)
		}
	}

	return err
}

func (cm *ConnectionManager) Close() {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	if cm.conn != nil {
		cm.conn.Close()
		cm.conn = nil
	}
}

type FileClient struct {
	RealityAddr      string
	Alias            string
	connectionManage *ConnectionManager
	realityVersion   uint16
	realityID        uint32
	State            ConnectionState
}

func NewFileClient(realityAddr string, serverAlias string) (*FileClient, error) {
	log.Info("Creating file client, server address:", realityAddr)
	connetion, err := NewConnectionManager(realityAddr)
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
		return err
	}
	c.State = Online
	log.Info("Reconnected successfully")
	return nil
}

func (c *FileClient) Reverify() error {
	conn, err := c.connectionManage.GetConnection()
	if err != nil {
		return fmt.Errorf("reverify failed to get connection: %w", err)
	}
	if err := sendMessage(conn, MsgTypeReverify, nil); err != nil {
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
	// 重连后必须还是原来那台服务器（ID 与首次握手记录的一致），否则本地缓存的目录树不可信
	if reverifyResponse.Version != c.realityVersion || reverifyResponse.ServerID != c.realityID {
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
	// 握手用于端口探测，对端可能是不应答的陌生服务，必须限时
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetDeadline(time.Time{})

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
		return fmt.Errorf("failed to send ping message: %w", err)
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
		return fmt.Errorf("failed to decode pong message: %w", err)
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
		return nil, fmt.Errorf("%w: failed to get connection: %v", appError.ErrConnection, err)
	}
	request := TreeRequestMessage{
		PathLength: uint16(len(rootPath)),
		RootPath:   rootPath,
	}
	requestBytes := encodeTreeRequest(request)
	realityAddr := conn.RemoteAddr().String()
	if err := sendMessage(conn, MsgTypeTreeRequest, requestBytes); err != nil {
		return nil, fmt.Errorf("%w: failed to send tree request: %v", appError.ErrConnection, err)
	}
	log.Debugf("Sent tree request to %s for path: %s", realityAddr, rootPath)
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to receive message: %v", appError.ErrConnection, err)
	}
	if msgType == MsgTypeError {
		errorMsg, err := decodeErrorMessage(bodyBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: failed to decode error message: %v", appError.ErrConnection, err)
		}

		return nil, fmt.Errorf("reality error: %s", errorMsg.ErrorMessage)
	}
	if msgType != MsgTypeTreeResponse {
		return nil, fmt.Errorf("invalid tree response message type, got %d", msgType)
	}
	treeResponse, err := decodeTreeResponse(bodyBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to decode tree response: %v", appError.ErrConnection, err)
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
		return "", fmt.Errorf("%w: failed to get connection: %v", appError.ErrConnection, err)
	}
	requestFile := FileRequestMessage{
		PathLength: uint16(len(filePath)),
		FilePath:   filePath,
		Offset:     0,
	}
	requestBytes := encodeFileRequest(requestFile)
	if err := sendMessage(conn, MsgTypeFileRequest, requestBytes); err != nil {
		return "", fmt.Errorf("%w: failed to send file request: %v", appError.ErrConnection, err)
	}

	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		return "", fmt.Errorf("%w: failed to receive message: %v", appError.ErrConnection, err)
	}

	if msgType == MsgTypeError {
		errorMsg, err := decodeErrorMessage(bodyBytes)
		if err != nil {
			return "", fmt.Errorf("%w: failed to decode error message: %v", appError.ErrConnection, err)
		}
		return "", fmt.Errorf("reality error: %s", errorMsg.ErrorMessage)
	}

	if msgType != MsgTypeFileResponse {
		return "", fmt.Errorf("invalid file response message type, got %d", msgType)
	}
	fileResponse, err := decodeFileResponse(bodyBytes)
	if err != nil {
		return "", fmt.Errorf("%w: failed to decode file response: %v", appError.ErrConnection, err)
	}

	fullPath := filepath.Join(config.StartPath, filePath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create directory for file: %w", err)
	}
	// 先写入同目录下的临时文件，校验通过后原子改名覆盖，
	// 避免下载中断/哈希不匹配时留下半截目标文件
	tmpFile, err := os.CreateTemp(filepath.Dir(fullPath), ".local-mirror-dl-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		tmpFile.Close()
		os.Remove(tmpPath)
	}()

	sessionID := fileResponse.SessionID
	receivedSize := uint64(0)
	startTime := time.Now()

	for {
		msgType, bodyBytes, err := receiveMessage(conn)
		if err != nil {
			return "", fmt.Errorf("%w: failed to receive message: %v", appError.ErrConnection, err)
		}
		switch msgType {
		case MsgTypeFileData:
			dataMsg, err := decodeFileData(bodyBytes)
			if err != nil {
				return "", fmt.Errorf("%w: error decoding file data message: %v", appError.ErrConnection, err)
			}
			if dataMsg.SessionID != sessionID {
				return "", fmt.Errorf("invalid session ID in file data message, got %x", dataMsg.SessionID)
			}

			if _, err := tmpFile.Write(dataMsg.Data); err != nil {
				return "", fmt.Errorf("error writing file data: %w", err)
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
			log.Debugf("Sent acknowledge message, session ID: %x, offset: %d", sessionID, receivedSize)
		case MsgTypeFileComplete:
			completeMsg, err := decodeFileComplete(bodyBytes)
			if err != nil {
				return "", fmt.Errorf("%w: error decoding file complete message: %v", appError.ErrConnection, err)
			}
			if completeMsg.SessionID != sessionID {
				return "", fmt.Errorf("invalid session ID in file complete message, got %x", completeMsg.SessionID)
			}

			if err := tmpFile.Sync(); err != nil {
				log.Warnf("file.Sync() failed for %s: %v", tmpPath, err)
			}
			if err := tmpFile.Close(); err != nil {
				return "", fmt.Errorf("error closing file: %w", err)
			}

			fileHash, err := utils.CalcBlake3(tmpPath)
			if err != nil {
				return "", fmt.Errorf("error calculating file hash: %w", err)
			}
			if fileHash != completeMsg.FileHash {
				return "", fmt.Errorf("file hash mismatch, expected %x, got %x", completeMsg.FileHash, fileHash)
			}
			if err := os.Rename(tmpPath, fullPath); err != nil {
				return "", fmt.Errorf("error renaming temp file to %s: %w", fullPath, err)
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

// GetTreeChange 查询服务端在 [startTime, endTime] 时间段内发生变更的目录。
// 时间窗游标由调用方维护，保证前后两次查询无缝衔接，不丢变更。
func (c *FileClient) GetTreeChange(startTime, endTime int64) ([]string, error) {
	conn, err := c.connectionManage.GetConnection()
	if err != nil {
		return nil, fmt.Errorf("%w: failed to get connection: %v", appError.ErrConnection, err)
	}
	request := RecentChangeRequestMessage{
		ClientID:  config.InstanceID,
		startTime: startTime,
		endTime:   endTime,
	}
	requestBytes := encodeRecentChangeRequest(request)
	if err := sendMessage(conn, MsgTypeRecentChangeRequest, requestBytes); err != nil {
		return nil, fmt.Errorf("%w: failed to send recent change request: %v", appError.ErrConnection, err)
	}
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to receive message: %v", appError.ErrConnection, err)
	}
	if msgType == MsgTypeError {
		errorMsg, err := decodeErrorMessage(bodyBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: failed to decode error message: %v", appError.ErrConnection, err)
		}
		return nil, fmt.Errorf("reality error: %s", errorMsg.ErrorMessage)
	}
	if msgType != MsgTypeRecentChangeResponse {
		return nil, fmt.Errorf("invalid recent change response message type, got %d", msgType)
	}
	recentChangeResponse, err := decodeRecentChangeResponse(bodyBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to decode recent change response: %v", appError.ErrConnection, err)
	}
	if len(recentChangeResponse.Changes) == 0 {
		log.Warnf("Received empty recent change response from %s", c.RealityAddr)
		return []string{}, nil
	}
	log.Infof("Received recent change response from %s, changes count: %d",
		c.RealityAddr,
		len(recentChangeResponse.Changes))
	log.Debugf("Received recent change response: %v", recentChangeResponse.Changes)
	return recentChangeResponse.Changes, nil

}
