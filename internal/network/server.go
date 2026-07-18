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
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// LongPollHold 变更长轮询的服务端最大挂起时长。
// 无变更时到点返回空响应作为保活，客户端立即重新发起
const LongPollHold = 50 * time.Second

// ClientIdleTimeout 服务端判定客户端失联的空闲阈值。
// 客户端长轮询每 ≤LongPollHold 就有一次往返，90 秒覆盖两个挂起周期；
// 同时覆盖"建立了 TCP 连接但从不发消息"的僵尸连接
const ClientIdleTimeout = 90 * time.Second

// maxConcurrentConnections 服务端同时处理的连接数上限。
// 未设 -k 时握手无认证，无上限会被无限开连接耗尽 goroutine/内存；
// 达上限后新连接直接拒绝（关闭），已有连接不受影响。局域网多客户端
// 场景 256 足够宽裕
const maxConcurrentConnections = 256

// treePageMaxEntries 目录树响应单页条目上限。每条目 JSON 约 250 字节，
// 两万条约 5 MB，远低于消息体上限（64 MB）；超出的条目经 ContinueFrom
// 续页游标分多次请求，消除超大目录的确定性失败
const treePageMaxEntries = 20000

// changeFullResyncThreshold 变更响应降级阈值。单次区间查询命中的变更目录
// 超过此数时不再下发列表，改为 FullResync 信号让客户端全量对账——
// 既避免响应逼近消息体上限（此前是确定性失败 + 最长 1 小时的重连活锁），
// 也因为处理上万条目录 diff 本就比一次全量扫描更慢
const changeFullResyncThreshold = 8192

type fileServer struct {
	listener  net.Listener
	clientMap sync.Map
	// connSlots 带缓冲的信号量，容量即连接上限；每条连接占一个槽，
	// handleConnection 退出时释放
	connSlots chan struct{}
}

// ListenAvailable 从 basePort 开始逐个尝试监听，返回第一个可用端口。
// 提前绑定好 listener 再交给 fileServer，调用方（启动横幅）才能拿到真实端口
func ListenAvailable(basePort, count int) (net.Listener, int, error) {
	var lastErr error
	for port := basePort; port < basePort+count; port++ {
		// 强制 tcp4：macOS 上 "tcp" 会退化为 IPv6 双栈套接字，
		// 与已被占用的 IPv4 端口"共存"，导致客户端（IPv4 拨号）连不到本服务
		listener, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
		if err == nil {
			if port != basePort {
				log.Warnf("port %d unavailable, using %d", basePort, port)
			}
			return listener, port, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf("no free port in range %d-%d: %w", basePort, basePort+count-1, lastErr)
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
	SessionMap     sync.Map  // 活跃的会话列表
}

func (c *client) UpdateLastActiveTime() {
	c.LastActiveTime = time.Now()
}

// removeClientIfCurrent 仅当 clientMap 中该 ID 当前对应的仍是 expected 这个
// client 对象时才删除。
//
// 背景：clientMap 以客户端 InstanceID 为键。同一个客户端进程快速断线重连时
// （InstanceID 不变），新连接握手后会 Store 一个新的 client 对象覆盖旧的；
// 但旧连接对应的 goroutine 可能因为迟迟才检测到自己已失效（例如还在阻塞地
// 尝试发送文件数据），在那之后才执行清理逻辑——如果直接无条件 Delete(ID)，
// 删掉的其实是新连接刚注册的条目，导致新连接被服务端误判为"找不到客户端"
// 而遭到关闭。必须用原子的 CompareAndDelete：Load 后再 Delete 的两步写法
// 在两步之间仍可能被新连接的 Store 插入，竞态只是变窄而没有消除。
func (s *fileServer) removeClientIfCurrent(id uint32, expected *client) {
	s.clientMap.CompareAndDelete(id, expected)
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

func NewFileServer(listener net.Listener) *fileServer {
	log.Info("Creating file server, listen address:", listener.Addr())
	return &fileServer{
		listener:  listener,
		clientMap: sync.Map{},
		connSlots: make(chan struct{}, maxConcurrentConnections),
	}
}

func (s *fileServer) Start() error {
	log.Infof("File server started on %s", s.listener.Addr())
	defer s.listener.Close()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			log.Error("Error accepting connection:", err)
			continue
		}
		// 连接数上限：非阻塞获取槽位，满则直接拒绝，避免无认证时被无限
		// 开连接耗尽资源。槽位在 handleConnection 退出时释放
		select {
		case s.connSlots <- struct{}{}:
		default:
			log.Warnf("concurrent connection cap %d reached, rejecting %s", maxConcurrentConnections, conn.RemoteAddr())
			conn.Close()
			continue
		}
		// 长轮询挂起期间连接静默，keepalive 帮助检测死客户端
		enableKeepAlive(conn)
		go s.handleConnection(conn)
	}
}

func (s *fileServer) handleConnection(conn net.Conn) {
	// 释放连接槽位（与 Start 中的获取配对）；置于最前，任何退出路径都归还
	defer func() { <-s.connSlots }()

	clientAddr := conn.RemoteAddr().String()
	log.Infof("Client connected from %s to local port %s", clientAddr, conn.LocalAddr().String())

	// 配置了口令则先完成 Noise 加密握手，之后的所有协议消息透明加解密；
	// 口令不一致或对端未加密时在这里直接拒绝
	if *config.Secret != "" {
		secured, err := SecureConn(conn, *config.Secret, false)
		if err != nil {
			log.Warnf("Rejecting %s: %v", clientAddr, err)
			conn.Close()
			return
		}
		conn = secured
	}
	client := &client{
		ID:             0,
		Alias:          "",
		Addr:           clientAddr,
		Role:           0,
		LastActiveTime: time.Now(),
		Version:        0,
		Connected:      false,
		Conn:           conn,
		SessionMap:     sync.Map{},
	}

	defer func() {
		if err := conn.Close(); err != nil {
			log.Error(err)
		}
		s.removeClientIfCurrent(client.ID, client)
	}()

	for {
		// 每轮收消息前重置读超时：超过空闲阈值没有任何消息（包括心跳）
		// 即认为客户端失联，关闭连接释放资源
		if err := conn.SetReadDeadline(time.Now().Add(ClientIdleTimeout)); err != nil {
			log.Errorf("Failed to set read deadline for %s: %v", clientAddr, err)
			return
		}
		msgType, bodyBytes, err := receiveMessage(conn)
		if err != nil {
			switch {
			case errors.Is(err, os.ErrDeadlineExceeded):
				log.Warnf("Client %s idle for over %v, closing connection", clientAddr, ClientIdleTimeout)
			case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
				log.Warnf("Client %s disconnected", clientAddr)
			default:
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
		case MsgTypeRecentChangeRequest:
			if closed := s.dispatchError(conn, client, s.handleRecentChangeRequest(client.ID, bodyBytes)); closed {
				return
			}
		case MsgTypeTreeRequest:
			if closed := s.dispatchError(conn, client, s.handleTreeRequest(client.ID, bodyBytes)); closed {
				return
			}
		case MsgTypeFileRequest:
			if closed := s.dispatchError(conn, client, s.handleFileRequest(client.ID, bodyBytes)); closed {
				return
			}
		default:
			log.Errorf("Unknown message type: %d", msgType)
		}

	}
}

// dispatchError 统一处理 handler 返回的错误：连接类错误关闭连接并注销
// 客户端（返回 true 告知调用方退出读循环）；业务类错误编码为结构化
// Error 消息下发（wireError 携带的码原样透传，未归类错误落 ErrCodeInternal）
func (s *fileServer) dispatchError(conn net.Conn, c *client, err error) (closed bool) {
	if err == nil {
		return false
	}
	log.Error(err)
	if errors.Is(err, appError.ErrConnection) {
		conn.Close()
		s.removeClientIfCurrent(c.ID, c)
		log.Warnf("Connection closed for %s due to error: %v", c.Addr, err)
		return true
	}
	msg := ErrorMessage{Code: ErrCodeInternal, Message: err.Error()}
	var we *wireError
	if errors.As(err, &we) {
		msg = ErrorMessage{Code: we.Code, Path: we.Path, Message: we.Message}
	}
	if serr := sendMessage(conn, MsgTypeError, encodeErrorMessage(msg)); serr != nil {
		log.Error("Error sending error response:", serr)
	}
	return false
}

// pageTreeEntries 对目录条目按路径排序后取一页。continueFrom 为空取首页，
// 否则从严格大于 continueFrom 的条目开始；next 非空表示还有后续页。
// 页间目录内容可能变化（条目增删导致个别条目漏过一页），由变更推送与
// 全量扫描安全网兜底，与 diff 引擎的既有容错一致
func pageTreeEntries(entries []tree.Node, continueFrom string, limit int) (page []tree.Node, next string) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	start := 0
	if continueFrom != "" {
		start = sort.Search(len(entries), func(i int) bool { return entries[i].Path > continueFrom })
	}
	end := start + limit
	if end >= len(entries) {
		return entries[start:], ""
	}
	return entries[start:end], entries[end-1].Path
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
	log.Infof("Received tree request from %s for path: %s (cursor %q)", clientAddr, treeRequest.RootPath, treeRequest.ContinueFrom)
	treeLeaf, err := tree.GetDirContents(treeRequest.RootPath)
	if err != nil {
		return &wireError{Code: ErrCodeNotFound, Path: treeRequest.RootPath,
			Message: fmt.Sprintf("error getting tree contents: %v", err)}
	}
	page, next := pageTreeEntries(treeLeaf, treeRequest.ContinueFrom, treePageMaxEntries)
	for i := range page {
		page[i].ID = ""
		page[i].ParentID = ""
		// 节点路径随 JSON 进入线格式，统一转为 "/"（见 protocol.go 线格式约定）
		page[i].Path = filepath.ToSlash(page[i].Path)
	}
	treeData, err := json.Marshal(page)
	if err != nil {
		return fmt.Errorf("error marshalling tree leaf for path %s: %v", treeRequest.RootPath, err)
	}
	treeResponse := TreeResponseMessage{
		ContinueFrom: next,
		DataLength:   uint32(len(treeData)),
		Data:         treeData,
	}
	responseBytes := encodeTreeResponse(treeResponse)
	if err := sendMessage(conn, MsgTypeTreeResponse, responseBytes); err != nil {
		return fmt.Errorf("%w, error sending tree response for path %s: %v", appError.ErrConnection, treeRequest.RootPath, err)
	}
	log.Infof("Sent tree response to %s for path: %s, %d entries, %d bytes, more=%v",
		clientAddr, treeRequest.RootPath, len(page), len(treeData), next != "")
	return nil
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
	// 防止路径穿越：请求路径解析后必须仍位于同步根目录内
	if rel, err := filepath.Rel(config.StartPath, fullPath); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return &wireError{Code: ErrCodeOutOfRoot, Path: fileRequest.FilePath, Message: "illegal file path (escapes sync root)"}
	}
	// 纵深防御：即使目录树里不该出现符号链接，也拒绝对符号链接的请求，
	// 杜绝解引用读取同步根目录之外的文件（Lstat 不追踪链接本身）
	if linfo, lerr := os.Lstat(fullPath); lerr == nil && linfo.Mode()&os.ModeSymlink != 0 {
		return &wireError{Code: ErrCodeOutOfRoot, Path: fileRequest.FilePath, Message: "refusing to serve symlink"}
	}
	fileInfo, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &wireError{Code: ErrCodeNotFound, Path: fileRequest.FilePath, Message: "file not found"}
		}
		return fmt.Errorf("error getting file info: %s :%v", fileRequest.FilePath, err)

	} else {
		// 错误带上系统级原因（如 permission denied），它会随结构化错误应答
		// 发给客户端——对端日志里能直接看到失败根因，不用两头对日志；
		// 权限类失败带 ErrCodePermissionDenied，客户端据此跳过而非反复重试。
		// 读取失败同时登记进不可读列表，恢复可读后由 watcher 恢复循环补哈希
		fileHash, err := utils.CalcBlake3(fullPath)
		if err != nil {
			tree.MarkUnreadable(fullPath)
			if os.IsPermission(err) {
				return &wireError{Code: ErrCodePermissionDenied, Path: fileRequest.FilePath,
					Message: fmt.Sprintf("error calculating file hash: %v", err)}
			}
			return fmt.Errorf("error calculating file hash for %s: %v", fileRequest.FilePath, err)
		}

		file, err := os.Open(fullPath)
		if err != nil {
			if os.IsPermission(err) {
				return &wireError{Code: ErrCodePermissionDenied, Path: fileRequest.FilePath,
					Message: fmt.Sprintf("error opening file: %v", err)}
			}
			return fmt.Errorf("error opening file %s: %v", fileRequest.FilePath, err)
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

		_client.(*client).SessionMap.Store(session.ID, session)

		fileResponse := FileResponseMessage{
			SessionID: sessionBytes,
			FileSize:  uint64(fileInfo.Size()),
			FileHash:  fileHash,
		}
		responseBytes := encodeFileResponse(fileResponse)
		if err := sendMessage(conn, MsgTypeFileResponse, responseBytes); err != nil {
			s.removeClientIfCurrent(ID, _client.(*client))
			return fmt.Errorf("%w, error sending file response for %s", appError.ErrConnection, fileRequest.FilePath)
		}
		log.Debugf("Sent file response: session ID: %s, file size: %d bytes", sessionID, fileInfo.Size())
		if err := s.sendFileData(ID, session); err != nil {
			return err
		}
		return nil
	}
}

func (s *fileServer) sendFileData(ID uint32, session *session) error {
	_client, ok := s.clientMap.Load(ID)
	if !ok {
		return fmt.Errorf("%w, client not found for ID: %d", appError.ErrConnection, ID)
	}
	conn := _client.(*client).Conn
	// session.file 由 handleFileRequest 中的 defer 统一关闭，这里不重复 Close
	defer _client.(*client).SessionMap.Delete(session.ID)

	fileBuf := make([]byte, *config.FileBufferSize)
	for {
		n, err := session.file.Read(fileBuf)
		if n > 0 {
			dataMsg := FileDataMessage{
				SessionID:  session.ID,
				DataLength: uint32(n),
				Data:       fileBuf[:n],
			}
			if err := sendMessage(conn, MsgTypeFileData, encodeFileData(dataMsg)); err != nil {
				return fmt.Errorf("%w, error sending file data for %s", appError.ErrConnection, strings.Replace(session.FilePath, config.StartPath, ".", 1))
			}
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
	if err := sendMessage(conn, MsgTypeFileComplete, completeBytes); err != nil {
		return fmt.Errorf("%w, error sending file complete for %s", appError.ErrConnection, strings.Replace(session.FilePath, config.StartPath, ".", 1))
	}
	log.Infof("Sent file complete message: file path: %s", strings.Replace(session.FilePath, config.StartPath, ".", 1))
	return nil
}

func (s *fileServer) handleHandshake(conn net.Conn, bodyBytes []byte) (*HandshakeMessage, error) {
	// 版本拒绝前先回一条结构化错误再断连：对端（v3+）能在自己的日志里
	// 看到人话，而不是一个原因不明的 EOF。解码失败多半是旧版（v2 及更早，
	// 消息体更短）或非本协议流量，同样按版本不符应答——旧端不认识结构化
	// 错误也无妨，服务端日志里有完整记录
	rejectVersion := func(detail string) {
		msg := ErrorMessage{Code: ErrCodeVersionMismatch,
			Message: fmt.Sprintf("server %08x requires protocol [%d,%d]; %s",
				config.InstanceID, config.MinProtocolVersion, config.ProtocolVersion, detail)}
		_ = sendMessage(conn, MsgTypeError, encodeErrorMessage(msg))
	}

	handshakeMsg, err := decodeHandshake(bodyBytes)
	if err != nil {
		rejectVersion("handshake undecodable (peer probably runs protocol v2 or older)")
		return nil, fmt.Errorf("%w, failed to decode handshake: %w", appError.ErrConnection, err)
	}
	// 会话版本 = 两端 [Min, Version] 区间交集的最高值；交集为空即拒绝。
	// 当前两端区间都是 [3,3]，行为与严格相等一致（见 protocol.go 线格式约定）
	agreed, ok := negotiateVersion(config.ProtocolVersion, config.MinProtocolVersion,
		handshakeMsg.Version, handshakeMsg.MinVersion)
	if !ok {
		rejectVersion(fmt.Sprintf("client offered [%d,%d]", handshakeMsg.MinVersion, handshakeMsg.Version))
		return nil, fmt.Errorf("%w, protocol version mismatch: server=[%d,%d], client=[%d,%d]",
			appError.ErrConnection, config.MinProtocolVersion, config.ProtocolVersion,
			handshakeMsg.MinVersion, handshakeMsg.Version)
	}
	log.Infof("Received handshake message: version: %d (agreed %d), clientID: %d",
		handshakeMsg.Version, agreed, handshakeMsg.UUID)
	receiveHandshake := HandshakeMessage{
		Version:     config.ProtocolVersion,
		MinVersion:  config.MinProtocolVersion,
		UUID:        config.InstanceID,
		Role:        config.ModeMap[*config.Mode],
		FeatureBits: 0,
	}
	handshakeBytes := encodeHandshake(receiveHandshake)
	if err := sendMessage(conn, MsgTypeHandshake, handshakeBytes); err != nil {
		return nil, fmt.Errorf("%w, error sending handshake message: %v", appError.ErrConnection, err)
	}
	return &handshakeMsg, nil
}

// buildRecentChangeResponse 组装变更响应：数量超过阈值时降级为 FullResync
// 信号（列表省略）。纯函数，便于单元测试
func buildRecentChangeResponse(changes []string, now int64) RecentChangeResponseMessage {
	unique := utils.UniqueStrings(changes)
	if len(unique) > changeFullResyncThreshold {
		return RecentChangeResponseMessage{
			ServerID:     config.InstanceID,
			CoveredUntil: now,
			FullResync:   true,
		}
	}
	return RecentChangeResponseMessage{
		ServerID:     config.InstanceID,
		CoveredUntil: now,
		Changes:      unique,
	}
}

func (s *fileServer) handleRecentChangeRequest(ID uint32, bodyBytes []byte) error {
	_client, ok := s.clientMap.Load(ID)
	if !ok {
		// 与其余 handler 一致：未握手/已注销的连接按连接错误关闭，
		// 而不是静默不应答让对端干等读超时
		return fmt.Errorf("%w, client not found for ID: %d", appError.ErrConnection, ID)
	}
	conn := _client.(*client).Conn
	recentChangeRequest, err := decodeRecentChangeRequest(bodyBytes)
	if err != nil {
		return fmt.Errorf("%w, error decoding recent change request: %v", appError.ErrConnection, err)
	}
	log.Debugf("Received recent change request from %s, client ID: %d, startTime: %d",
		conn.RemoteAddr().String(), recentChangeRequest.ClientID, recentChangeRequest.StartTime)

	// 长轮询：区间内已有变更立刻回（追赶/重连场景）；无变更则挂起，
	// 等到变更落库广播或挂满上限后返回。挂起期间不读 socket，
	// 上限兜底避免死连接常驻。上界用服务端当前时刻，随每次唤醒重新求值。
	start := recentChangeRequest.StartTime
	holdDeadline := time.Now().Add(LongPollHold)
	for {
		// 先取信号再查询：若广播发生在查询之后、select 之前，
		// 该 channel 已被 close，select 立即返回并重查，不会漏
		sig := tree.ChangeSignal()
		now := time.Now().Unix()
		recentChanges, err := tree.GetChangedDirs(start, now)
		if err != nil {
			log.Error("Error getting changed dirs:", err)
			recentChanges = nil
		}

		if len(recentChanges) > 0 || err != nil || !time.Now().Before(holdDeadline) {
			responseMsg := buildRecentChangeResponse(recentChanges, now)
			if serr := sendMessage(conn, MsgTypeRecentChangeResponse, encodeRecentChangeResponse(responseMsg)); serr != nil {
				return fmt.Errorf("%w, error sending recent change response: %v", appError.ErrConnection, serr)
			}
			log.Debugf("Sent recent change response to %s, changes count: %d, fullResync=%v",
				conn.RemoteAddr().String(), len(responseMsg.Changes), responseMsg.FullResync)
			return nil
		}

		select {
		case <-sig:
		case <-time.After(time.Until(holdDeadline)):
		}
	}
}
