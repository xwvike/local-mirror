package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/internal/status"
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

// ListenedDualStack 服务端实际监听栈（横幅展示用）：
// ListenAvailable 成功返回后置位，true = v4+v6 双栈，false = 仅 v4
var ListenedDualStack bool

// ipv6Supported 探测主机是否支持 IPv6（绑一次 v6 环回，结果缓存）
var ipv6Supported = sync.OnceValue(func() bool {
	l, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		return false
	}
	l.Close()
	return true
})

// ListenAvailable 从 basePort 开始逐个尝试监听，返回第一个可用端口。
// 提前绑定好 listener 再交给 fileServer，调用方（启动横幅）才能拿到真实端口。
// 双栈（公网化支柱 B）：每个端口显式绑 v4（0.0.0.0）与 v6（[::]，V6ONLY）
// 两个套接字，双绑同端口成功才算可用——不用 "tcp" 通配双栈套接字，macOS 上
// 它会与已被占用的 IPv4 端口"共存"，客户端 v4 拨号连到别的程序，破坏端口
// 扫描模型。主机无 v6 时退回仅 v4，照常服务
func ListenAvailable(basePort, count int) (net.Listener, int, error) {
	var lastErr error
	for port := basePort; port < basePort+count; port++ {
		l4, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			lastErr = err
			continue
		}
		if !ipv6Supported() {
			if port != basePort {
				log.Warnf("port %d unavailable, using %d", basePort, port)
			}
			ListenedDualStack = false
			return l4, port, nil
		}
		l6, err := net.Listen("tcp6", fmt.Sprintf("[::]:%d", port))
		if err != nil {
			// v6 侧被占：两栈必须同端口（客户端只扫一个端口号），放弃该端口
			l4.Close()
			lastErr = err
			continue
		}
		if port != basePort {
			log.Warnf("port %d unavailable, using %d", basePort, port)
		}
		ListenedDualStack = true
		return newMultiListener(l4, l6), port, nil
	}
	return nil, 0, fmt.Errorf("no free port in range %d-%d: %w", basePort, basePort+count-1, lastErr)
}

// multiListener 把多个 listener 合并成一个：每个底层 listener 一个 accept
// 协程，连接汇入同一通道。Close 关闭全部底层 listener 并让 Accept 返回
type multiListener struct {
	listeners []net.Listener
	results   chan acceptResult
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

type acceptResult struct {
	conn net.Conn
	err  error
}

func newMultiListener(listeners ...net.Listener) *multiListener {
	m := &multiListener{
		listeners: listeners,
		results:   make(chan acceptResult),
		done:      make(chan struct{}),
	}
	for _, l := range listeners {
		go func(l net.Listener) {
			for {
				conn, err := l.Accept()
				select {
				case m.results <- acceptResult{conn, err}:
				case <-m.done:
					if conn != nil {
						conn.Close()
					}
					return
				}
				if err != nil {
					return
				}
			}
		}(l)
	}
	return m
}

func (m *multiListener) Accept() (net.Conn, error) {
	select {
	case r := <-m.results:
		return r.conn, r.err
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *multiListener) Close() error {
	m.closeOnce.Do(func() {
		close(m.done)
		for _, l := range m.listeners {
			if err := l.Close(); err != nil && m.closeErr == nil {
				m.closeErr = err
			}
		}
	})
	return m.closeErr
}

// Addr 返回 v4 侧地址（横幅与日志以此为准）
func (m *multiListener) Addr() net.Addr { return m.listeners[0].Addr() }

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

// NewFileServerDial 源拨出格（--send --connect）的文件服务器：无监听器，
// 连接由 StartDial 主动建立
func NewFileServerDial() *fileServer {
	return &fileServer{
		clientMap: sync.Map{},
		connSlots: make(chan struct{}, maxConcurrentConnections),
	}
}

// dialFirstMessageTimeout 源拨出后限时等汇的首条消息。健康的汇 accept 后
// 立即发握手，等不到就多半是两端都配了 --send（都在等对方先说话）或
// 拨错了对象——把静默死等变成有诊断的快速失败
const dialFirstMessageTimeout = 15 * time.Second

// StartDial 源端拨出（四象限的「源拨 → 汇听」格）：向监听中的汇拨号，
// 连接就绪后在同一套源端消息循环（serveConn）上服务。协议报文与谁拨号
// 无关——汇在连接建立后仍先说话；Noise initiator 自动跟拨号方（dialConn）。
// 重连与退避归拨号方，本函数阻塞不返回
func (s *fileServer) StartDial(addr string) {
	const baseDelay, maxDelay = 3 * time.Second, 60 * time.Second
	delay := baseDelay
	for {
		conn, err := dialConn(addr)
		if err != nil {
			log.Warnf("dial sink %s failed: %v (retrying in %v)", addr, err, delay)
			time.Sleep(delay)
			delay = min(delay*2, maxDelay)
			continue
		}

		// 限时等汇先说话（keepalive 已在 dialConn 内开启），
		// 等到的首条消息带进消息循环
		if derr := conn.SetReadDeadline(time.Now().Add(dialFirstMessageTimeout)); derr != nil {
			conn.Close()
			continue
		}
		msgType, body, err := receiveMessage(conn)
		if err != nil {
			log.Warnf("sink %s did not speak within %v (a healthy sink handshakes immediately; "+
				"are both ends configured --send, or is this the wrong peer?): %v",
				addr, dialFirstMessageTimeout, err)
			conn.Close()
			time.Sleep(delay)
			delay = min(delay*2, maxDelay)
			continue
		}
		if derr := conn.SetReadDeadline(time.Time{}); derr != nil {
			conn.Close()
			continue
		}

		delay = baseDelay
		log.Infof("Connected out to sink %s, serving", addr)
		s.serveConn(conn, &prereadMessage{msgType: msgType, body: body})
		log.Warnf("connection to sink %s ended, redialing", addr)
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
	s.serveConn(conn, nil)
}

// prereadMessage 已经从连接上读出、待交给消息循环处理的首条消息
// （源拨出格在进入循环前限时等汇先说话，读到的那条从这里带入）
type prereadMessage struct {
	msgType uint16
	body    []byte
}

// serveConn 在一条已就绪（必要时已加密）的连接上跑源端消息循环，
// 连接断开或对端失联时返回。与连接的建立方式无关：accept 到的连接
// （handleConnection）与拨出的连接（StartDial）都架在这同一个循环上。
// first 非 nil 时先处理这条已读出的消息再进入常规收发
func (s *fileServer) serveConn(conn net.Conn, first *prereadMessage) {
	clientAddr := conn.RemoteAddr().String()
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

	// sessionCounted 确保这条连接在 status 里最多计一次 up/down：握手成功才
	// 算一个活跃 peer（未握手的裸 TCP 连接不计），退出时对偶归还
	sessionCounted := false
	defer func() {
		if err := conn.Close(); err != nil {
			log.Error(err)
		}
		s.removeClientIfCurrent(client.ID, client)
		if sessionCounted {
			status.SessionDown()
		}
	}()

	for {
		var msgType uint16
		var bodyBytes []byte
		var err error
		if first != nil {
			msgType, bodyBytes = first.msgType, first.body
			first = nil
		} else {
			// 每轮收消息前重置读超时：超过空闲阈值没有任何消息（包括心跳）
			// 即认为客户端失联，关闭连接释放资源
			if derr := conn.SetReadDeadline(time.Now().Add(ClientIdleTimeout)); derr != nil {
				log.Errorf("Failed to set read deadline for %s: %v", clientAddr, derr)
				return
			}
			msgType, bodyBytes, err = receiveMessage(conn)
		}
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
			if !sessionCounted {
				sessionCounted = true
				status.SessionUp(fmt.Sprintf("serving %s", clientAddr))
			}
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
		status.RecordError()
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
	rel := strings.Replace(session.FilePath, config.StartPath, ".", 1)
	var sent uint64
	for {
		n, err := session.file.Read(fileBuf)
		if n > 0 {
			dataMsg := FileDataMessage{
				SessionID:  session.ID,
				DataLength: uint32(n),
				Data:       fileBuf[:n],
			}
			if err := sendMessage(conn, MsgTypeFileData, encodeFileData(dataMsg)); err != nil {
				return fmt.Errorf("%w, error sending file data for %s", appError.ErrConnection, rel)
			}
			// 进度上报（--status 实时展示）：节流在 status 内部
			sent += uint64(n)
			status.RecordProgress(rel, sent, session.FileSize)
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
	status.RecordFile(strings.Replace(session.FilePath, config.StartPath, ".", 1), session.FileSize)
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
	// 方向互补校验（四象限）：本端是送数据的源，对端申报的 Role 必须不是
	// send。老值平滑映射（mirror 一直发 2 = receive），旧 relay 的遗留值 3
	// 放行（它拨上游的这条连接确实在收）。结构化错误让对端日志里有人话
	if handshakeMsg.Role == config.RoleSend {
		msg := ErrorMessage{Code: ErrCodeDirectionConflict,
			Message: fmt.Sprintf("direction conflict: this end sends (source), peer %08x also declares send — exactly one end must be --receive",
				handshakeMsg.UUID)}
		_ = sendMessage(conn, MsgTypeError, encodeErrorMessage(msg))
		return nil, fmt.Errorf("%w, direction conflict: both ends declare send (peer %08x)",
			appError.ErrConnection, handshakeMsg.UUID)
	}
	log.Infof("Received handshake message: version: %d (agreed %d), clientID: %d",
		handshakeMsg.Version, agreed, handshakeMsg.UUID)
	// Role 承载本连接端点的数据方向：源引擎恒申报 send（relay 的下游侧
	// 也是送）。老 reality 值恰为 1 = send，对旧客户端零变化
	receiveHandshake := HandshakeMessage{
		Version:     config.ProtocolVersion,
		MinVersion:  config.MinProtocolVersion,
		UUID:        config.InstanceID,
		Role:        config.RoleSend,
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
