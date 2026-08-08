package network

import (
	"errors"
	"fmt"
	"io"
	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/internal/status"
	"net"
	"os"
	"runtime"
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

// maxConcurrentFileServes 全局并发文件服务（预哈希 + 传输）上限，与连接数上限解耦（5.4）。
// 整文件预哈希是 CPU 密集、传输是磁盘读密集，且二者对同一文件是两次全盘读；256 连接各自
// 触发大文件会放大成磁盘/CPU 打满（未认证监听时尤甚——SEC-01 已要求监听端带密钥堵住该最坏面，
// 这里再加全局并发上限兜底）。取 NumCPU 兼顾 CPU 侧，夹在 [4,16]：小机器不过低、大机器不过度
// 并发读拖垮磁盘（尤其机械盘）。轻量交互（握手/目录树/变更长轮询）不受此限
var maxConcurrentFileServes = min(max(runtime.NumCPU(), 4), 16)

// fileServeSlots 全局文件服务信号量，容量即上限。handleFileRequest 在预哈希前获取、出函数释放
var fileServeSlots = make(chan struct{}, maxConcurrentFileServes)

// acquireFileServeSlot 获取一个全局文件服务槽（阻塞至有空位），返回释放函数（5.4）。
// 调用方 `release := acquireFileServeSlot(); defer release()`，跨哈希+传输整段持有
func acquireFileServeSlot() (release func()) {
	fileServeSlots <- struct{}{}
	return func() { <-fileServeSlots }
}

type fileServer struct {
	listener  net.Listener
	clientMap sync.Map
	// connSlots 带缓冲的信号量，容量即连接上限；每条连接占一个槽，
	// handleConnection 退出时释放
	connSlots chan struct{}
}

type client struct {
	ID             uint32       // 客户端ID
	Alias          string       // 客户端别名
	Addr           string       // 客户端地址
	Role           uint8        // 客户端角色
	LastActiveTime time.Time    // 最后一次通讯时间
	Version        uint16       // 客户端协议版本
	Connected      bool         // 当前是否已连接
	Conn           net.Conn     // 客户端连接
	SessionMap     sync.Map     // 活跃的会话列表
	dirCache       *dirSnapshot // 分页遍历的已排序目录快照（PERF-01），仅本客户端消息循环访问
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
	var we *wireError
	isWire := errors.As(err, &we)
	// "查不到"是对查询的正常应答而非故障：客户端按变更日志或分页快照来访时，
	// 目录/文件可能已被删除。按 error 记会把常规的创建后删除刷成满屏告警
	if isWire && we.Code == ErrCodeNotFound {
		log.Debug(err)
	} else {
		log.Error(err)
	}
	if errors.Is(err, appError.ErrConnection) {
		conn.Close()
		s.removeClientIfCurrent(c.ID, c)
		status.RecordError()
		log.Warnf("Connection closed for %s due to error: %v", c.Addr, err)
		return true
	}
	msg := ErrorMessage{Code: ErrCodeInternal, Message: err.Error()}
	if isWire {
		msg = ErrorMessage{Code: we.Code, Path: we.Path, Message: we.Message}
	}
	if serr := sendMessage(conn, MsgTypeError, encodeErrorMessage(msg)); serr != nil {
		log.Error("Error sending error response:", serr)
	}
	return false
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
