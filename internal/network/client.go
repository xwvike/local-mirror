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
	// 长轮询期间连接长时间静默，开启 TCP keepalive 让 OS 层更快发现死对端
	enableKeepAlive(conn)
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

	// 连接有效性由变更长轮询的往返保证（每 ≤LongPollHold 一次），
	// 读超时/服务端实例变化都会触发上层关闭并重建连接
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

// Reverify 用于 Reconnect 后重新验证连接：发送的是真正的 Handshake 消息
// （而不是原来的 MsgTypeReverify），因为 MsgTypeReverify 请求体是空的，
// 服务端无从得知是"哪个" InstanceID 的客户端在重连，也就无法把这个新的
// TCP 连接重新注册进 clientMap——此前 Reconnect 后的连接在服务端上
// 永远没有 clientMap 记录，导致 Reconnect 之后任何依赖 clientMap.Load
// 的请求（TreeRequest/FileRequest 等）都会被判定为"client not found"
// 而遭到服务端主动关闭，实测表现为反复 EOF、最终整个目录被放弃同步。
// 复用 MsgTypeHandshake 让服务端用已有的注册逻辑正确处理重连，客户端这边
// 仍然按原语义校验响应里的服务端信息是否与已知值一致（不一致说明连接到了
// 不同的服务器，本地缓存的目录树不可信，需要整体重建会话）。
func (c *FileClient) Reverify() error {
	conn, err := c.connectionManage.GetConnection()
	if err != nil {
		return fmt.Errorf("reverify failed to get connection: %w", err)
	}
	// 与 Handshake 相同的限时：重连时对端可能 TCP 可达但已不应答，
	// 不限时会让 Reconnect 无限期阻塞，整个同步循环挂死
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetDeadline(time.Time{})

	handshakeMsg := HandshakeMessage{
		Version: config.ProtocolVersion,
		UUID:    config.InstanceID,
		Role:    config.ModeMap[*config.Mode],
	}
	if err := sendMessage(conn, MsgTypeHandshake, encodeHandshake(handshakeMsg)); err != nil {
		return fmt.Errorf("failed to send reverify handshake: %w", err)
	}
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		return fmt.Errorf("failed to receive reverify response: %w", err)
	}
	if msgType != MsgTypeHandshake {
		return fmt.Errorf("invalid reverify response message type, got %d", msgType)
	}
	handshakeResponse, err := decodeHandshake(bodyBytes)
	if err != nil {
		return fmt.Errorf("failed to decode reverify response: %w", err)
	}
	// 重连后必须还是原来那台服务器（ID 与首次握手记录的一致），否则本地缓存的目录树不可信
	if handshakeResponse.Version != c.realityVersion || handshakeResponse.UUID != c.realityID {
		return fmt.Errorf("reverify failed, expected version %d and server ID %d, got version %d and server ID %d",
			c.realityVersion, c.realityID,
			handshakeResponse.Version, handshakeResponse.UUID)
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
	// 自连接防护：中继模式下客户端扫描 localhost 端口时可能撞上
	// 自己的服务端（镜像自己到自己），据 InstanceID 识别并跳过
	if handshakeResponse.UUID == config.InstanceID {
		return fmt.Errorf("connected to self (instance %08x), skipping", config.InstanceID)
	}
	// 协议版本必须一致（v2 起变更查询为长轮询语义）
	if handshakeResponse.Version != config.ProtocolVersion {
		return fmt.Errorf("protocol version mismatch: local=%d, server=%d",
			config.ProtocolVersion, handshakeResponse.Version)
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
				// 会话 ID 不符说明读到了错位/他人的数据流，连接已不可信，
				// 必须按连接错误处理触发关闭重连，否则脏字节会污染后续请求
				return "", fmt.Errorf("%w: invalid session ID in file data message, got %x", appError.ErrConnection, dataMsg.SessionID)
			}

			if _, err := file.Write(dataMsg.Data); err != nil {
				// 写入失败发生在数据流中间：服务端仍在发送剩余数据，
				// 此时提前返回会在连接里留下未消费的字节，后续请求将读到
				// 错位数据被误解析。标记为连接错误，让上层关闭并重建连接
				return "", fmt.Errorf("%w: error writing file data: %v", appError.ErrConnection, err)
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
				return "", fmt.Errorf("%w: invalid session ID in file complete message, got %x", appError.ErrConnection, completeMsg.SessionID)
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

// LongPollReadTimeout 客户端长轮询的读超时。
// 必须大于服务端挂起上限（network.LongPollHold），否则会在服务端
// 正常保活返回前误判超时
const LongPollReadTimeout = 60 * time.Second

// GetTreeChange 发起一次变更长轮询：请求自 startTime 起的变更，
// 服务端有变更立即返回、否则挂起至保活上限。返回变更目录列表与
// coveredUntil（服务端已覆盖到的时刻，调用方据此推进游标，杜绝重叠/遗漏）。
func (c *FileClient) GetTreeChange(startTime int64) (changes []string, coveredUntil int64, err error) {
	conn, err := c.connectionManage.GetConnection()
	if err != nil {
		return nil, 0, fmt.Errorf("%w: failed to get connection: %v", appError.ErrConnection, err)
	}
	// 读超时必须覆盖服务端挂起上限，挂起本身不算连接异常
	conn.SetReadDeadline(time.Now().Add(LongPollReadTimeout))
	defer conn.SetReadDeadline(time.Time{})

	request := RecentChangeRequestMessage{
		ClientID:  config.InstanceID,
		startTime: startTime,
		endTime:   time.Now().Unix(), // 服务端以自身时刻为上界，此字段仅作参考
	}
	requestBytes := encodeRecentChangeRequest(request)
	if err := sendMessage(conn, MsgTypeRecentChangeRequest, requestBytes); err != nil {
		return nil, 0, fmt.Errorf("%w: failed to send recent change request: %v", appError.ErrConnection, err)
	}
	msgType, bodyBytes, err := receiveMessage(conn)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: failed to receive message: %v", appError.ErrConnection, err)
	}
	if msgType == MsgTypeError {
		errorMsg, derr := decodeErrorMessage(bodyBytes)
		if derr != nil {
			return nil, 0, fmt.Errorf("%w: failed to decode error message: %v", appError.ErrConnection, derr)
		}
		return nil, 0, fmt.Errorf("reality error: %s", errorMsg.ErrorMessage)
	}
	if msgType != MsgTypeRecentChangeResponse {
		return nil, 0, fmt.Errorf("invalid recent change response message type, got %d", msgType)
	}
	resp, err := decodeRecentChangeResponse(bodyBytes)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: failed to decode recent change response: %v", appError.ErrConnection, err)
	}
	// 服务端实例变化（悄悄重启）→ 本地缓存树不可信，按连接错误触发会话重建
	if resp.ServerID != c.realityID {
		return nil, 0, fmt.Errorf("%w: server instance changed, expected %08x, got %08x",
			appError.ErrConnection, c.realityID, resp.ServerID)
	}
	if len(resp.Changes) > 0 {
		log.Infof("Received %d changed dirs from %s", len(resp.Changes), c.RealityAddr)
		log.Debugf("Changed dirs: %v", resp.Changes)
	}
	return resp.Changes, resp.CoveredUntil, nil
}
