package network

import (
	"encoding/json"
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

// dialConn 建立到服务端的连接；配置了口令时在 TCP 之上完成 Noise 加密握手
func dialConn(addr string) (net.Conn, error) {
	// 带超时拨号：端口扫描时不能在无响应的地址上无限期等待
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", addr, err)
	}
	if *config.Secret != "" {
		secured, err := SecureConn(conn, *config.Secret, true)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("%w %s: %v", ErrSecureHandshake, addr, err)
		}
		return secured, nil
	}
	return conn, nil
}

func NewConnectionManager(addr string) (*ConnectionManager, error) {
	conn, err := dialConn(addr)
	if err != nil {
		return nil, err
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

	// 连接的实际有效性由周期心跳（FileClient.Ping）保证，
	// 心跳失败时上层会关闭并重建连接
	if cm.conn != nil {
		return cm.conn, nil
	}
	return nil, fmt.Errorf("connection is invalid")
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

		cm.conn, err = dialConn(cm.connectAddr)
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

// Ping 发送一次心跳并等待应答，用于空闲期探活。
// 网络失败包装为 ErrConnection，上层据此触发重连；
// 协议是同步请求-响应模型，调用方必须保证与其他任务串行使用连接
func (c *FileClient) Ping() error {
	conn, err := c.connectionManage.GetConnection()
	if err != nil {
		return fmt.Errorf("%w: failed to get connection: %v", appError.ErrConnection, err)
	}
	// 对端可能已经死亡，探活必须限时
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetDeadline(time.Time{})

	pingMessage := HeartbeatPingMessage{
		Version:   config.ProtocolVersion,
		Timestamp: time.Now().Unix(),
		ClientID:  config.InstanceID,
	}
	pingBytes := encodeHeartbeatPing(pingMessage)
	if err := sendMessage(conn, MsgTypeHeartbeatPing, pingBytes); err != nil {
		return fmt.Errorf("%w: failed to send ping message: %v", appError.ErrConnection, err)
	}
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		return fmt.Errorf("%w: failed to receive pong message: %v", appError.ErrConnection, err)
	}
	if msgType == MsgTypeError {
		errorMsg, err := decodeErrorMessage(bodyBytes)
		if err != nil {
			return fmt.Errorf("failed to decode error message: %w", err)
		}
		return fmt.Errorf("server error: %s", errorMsg.ErrorMessage)
	}
	if msgType != MsgTypeHeartbeatPong {
		return fmt.Errorf("invalid pong message type, got %d", msgType)
	}
	pongMessage, err := decodeHeartbeatPong(bodyBytes)
	if err != nil {
		return fmt.Errorf("failed to decode pong message: %w", err)
	}
	// 服务端悄悄重启后 InstanceID 会变化，本地缓存的目录树不再可信，
	// 按连接错误处理触发整个会话重建（重新握手 + 全量扫描）
	if pongMessage.ServerID != c.realityID {
		return fmt.Errorf("%w: server instance changed, expected %08x, got %08x",
			appError.ErrConnection, c.realityID, pongMessage.ServerID)
	}
	log.Debugf("Received pong: version=%d timestamp=%d serverID=%08x",
		pongMessage.Version, pongMessage.Timestamp, pongMessage.ServerID)
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

// partialMeta 记录分片对应的服务端文件指纹。
// 续传前用它判断服务端文件是否在中断期间发生了变化
type partialMeta struct {
	Hash string `json:"hash"` // 服务端整文件 blake3（十六进制）
	Size uint64 `json:"size"` // 服务端文件大小
}

// partialPaths 返回某个同步路径对应的分片文件与元数据文件位置。
// 放在 .local-mirror/partial/ 下：该目录在忽略列表中，
// 不会被建树扫描收录，也不会被镜像 diff 当作多余文件删除；
// 文件名用路径摘要，保证长路径/特殊字符安全且可跨重试定位
func partialPaths(filePath string) (string, string) {
	key := utils.HashString(filePath)
	dir := filepath.Join(config.StartPath, ".local-mirror", "partial")
	return filepath.Join(dir, key+".part"), filepath.Join(dir, key+".meta")
}

// loadPartialState 读取上次中断留下的分片，返回可续传的起始偏移。
// 分片或元数据缺失/损坏都按无分片处理（从 0 开始）
func loadPartialState(partialPath, metaPath string) (uint64, *partialMeta) {
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return 0, nil
	}
	var meta partialMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return 0, nil
	}
	info, err := os.Stat(partialPath)
	if err != nil || info.Size() <= 0 || uint64(info.Size()) > meta.Size {
		return 0, nil
	}
	return uint64(info.Size()), &meta
}

func discardPartial(partialPath, metaPath string) {
	os.Remove(partialPath)
	os.Remove(metaPath)
}

// drainFileSession 把一次已经开始的文件传输会话读到结束并丢弃数据。
// 分片过期时服务端已按旧 offset 开始发送，这段数据无法拼装成完整文件，
// 但排空它可以保持连接可复用，避免为此断连重建
func drainFileSession(conn net.Conn) error {
	for {
		msgType, _, err := receiveMessage(conn)
		if err != nil {
			return err
		}
		switch msgType {
		case MsgTypeFileData:
			continue
		case MsgTypeFileComplete, MsgTypeError:
			return nil
		default:
			return fmt.Errorf("unexpected message type %d while draining file session", msgType)
		}
	}
}

func (c *FileClient) DownloadFile(filePath string) (string, error) {
	conn, err := c.connectionManage.GetConnection()
	if err != nil {
		return "", fmt.Errorf("%w: failed to get connection: %v", appError.ErrConnection, err)
	}

	partialPath, metaPath := partialPaths(filePath)
	if err := os.MkdirAll(filepath.Dir(partialPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create partial dir: %w", err)
	}
	offset, prevMeta := loadPartialState(partialPath, metaPath)

	requestFile := FileRequestMessage{
		PathLength: uint16(len(filePath)),
		FilePath:   filePath,
		Offset:     offset,
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
		// 服务端已无法提供该文件（如已被删除），分片不再有保留价值
		discardPartial(partialPath, metaPath)
		return "", fmt.Errorf("reality error: %s", errorMsg.ErrorMessage)
	}

	if msgType != MsgTypeFileResponse {
		return "", fmt.Errorf("invalid file response message type, got %d", msgType)
	}
	fileResponse, err := decodeFileResponse(bodyBytes)
	if err != nil {
		return "", fmt.Errorf("%w: failed to decode file response: %v", appError.ErrConnection, err)
	}
	serverHash := fmt.Sprintf("%x", fileResponse.FileHash)

	// 续传有效性：分片记录的服务端文件指纹必须与本次响应一致，
	// 否则服务端文件在中断期间变过，本次数据流是新文件的中段，无法拼接
	resume := offset > 0 && prevMeta != nil &&
		prevMeta.Hash == serverHash && prevMeta.Size == fileResponse.FileSize
	if offset > 0 && !resume {
		discardPartial(partialPath, metaPath)
		if err := drainFileSession(conn); err != nil {
			return "", fmt.Errorf("%w: failed to drain stale session: %v", appError.ErrConnection, err)
		}
		return "", fmt.Errorf("partial data for %s is stale, will restart from offset 0 on next attempt", filePath)
	}

	fullPath := filepath.Join(config.StartPath, filePath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create directory for file: %w", err)
	}

	var file *os.File
	if resume {
		file, err = os.OpenFile(partialPath, os.O_WRONLY|os.O_APPEND, 0644)
		log.Infof("断点续传 %s: 已有 %d/%d 字节", filePath, offset, fileResponse.FileSize)
	} else {
		file, err = os.Create(partialPath)
		if err == nil {
			// 先落 meta 再收数据：中断发生在任何时刻，分片都能被下次识别
			metaData, _ := json.Marshal(partialMeta{Hash: serverHash, Size: fileResponse.FileSize})
			if werr := os.WriteFile(metaPath, metaData, 0644); werr != nil {
				log.Warnf("Failed to write partial meta for %s: %v", filePath, werr)
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("failed to open partial file: %w", err)
	}
	// 只负责关闭；分片文件在传输失败时保留，供下次续传
	defer file.Close()

	sessionID := fileResponse.SessionID
	receivedSize := offset
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

			if _, err := file.Write(dataMsg.Data); err != nil {
				return "", fmt.Errorf("error writing file data: %w", err)
			}
			// 不逐块回发 Acknowledge：服务端流式发送期间不读取 socket，
			// 大文件的确认消息会填满对端接收缓冲，造成双向阻塞死锁；
			// 续传依据本地分片大小，不需要确认机制
			receivedSize += uint64(len(dataMsg.Data))
		case MsgTypeFileComplete:
			completeMsg, err := decodeFileComplete(bodyBytes)
			if err != nil {
				return "", fmt.Errorf("%w: error decoding file complete message: %v", appError.ErrConnection, err)
			}
			if completeMsg.SessionID != sessionID {
				return "", fmt.Errorf("invalid session ID in file complete message, got %x", completeMsg.SessionID)
			}

			if err := file.Sync(); err != nil {
				log.Warnf("file.Sync() failed for %s: %v", partialPath, err)
			}
			if err := file.Close(); err != nil {
				return "", fmt.Errorf("error closing file: %w", err)
			}

			// 无论是否续传，都对拼装后的整个文件做完整性校验
			fileHash, err := utils.CalcBlake3(partialPath)
			if err != nil {
				return "", fmt.Errorf("error calculating file hash: %w", err)
			}
			if fileHash != completeMsg.FileHash {
				// 分片已被证明损坏，保留只会反复失败
				discardPartial(partialPath, metaPath)
				return "", fmt.Errorf("file hash mismatch, expected %x, got %x", completeMsg.FileHash, fileHash)
			}
			if err := os.Rename(partialPath, fullPath); err != nil {
				return "", fmt.Errorf("error renaming partial file to %s: %w", fullPath, err)
			}
			os.Remove(metaPath)
			transferSpeed := float64(fileResponse.FileSize-offset) / time.Since(startTime).Seconds()
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
