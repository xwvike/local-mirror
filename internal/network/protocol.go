package network

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"path/filepath"

	log "github.com/sirupsen/logrus"
)

// ============================ 线格式约定 ============================
//
// 版本与协商（v3 起）：握手双方各自携带 [MinVersion, Version] 支持区间与
// FeatureBits 能力位，会话版本取两区间交集的最高值，交集为空则拒绝
// （服务端拒绝前回一条 ErrCodeVersionMismatch 错误，让对端日志里有人话）。
// 当前两端区间均为 [3,3]，行为与严格相等一致；该结构的意义在于未来版本
// 可以引入真正的跨版本协商而无需再次 flag-day。FeatureBits 当前恒为 0，
// 非零位留给未来能力声明（压缩、增量传输、PSK 拉伸参数等）。
//
// 同版本演进（正式机制）：解码器只读取已知字段、静默忽略消息体尾部的
// 多余字节。因此**在消息体尾部追加新字段是同版本内的兼容演进方式**：
// 新端写、旧端忽略；新字段必须自带存在性语义（长度前缀或哨兵值），
// 不得改变既有字段的顺序与宽度。改动既有字段仍需 bump 版本。
//
// 路径：协议中所有文件/目录路径一律以 "/" 为分隔符传输。各端进程内部
// （目录树库、变更日志、fsnotify、SafeJoin）始终使用本机
// filepath.Separator，转换只发生在编解码边界——encode 时 ToSlash，
// decode 时 FromSlash（Unix 上两者都是恒等变换）。
// 已知边缘：Unix 文件名中的字面反斜杠经线格式到达 Windows 端会被当作
// 分隔符展开成子目录——该名字本就无法在 Windows 文件系统表示，可接受。
//
// 交互模型：严格同步请求-响应，单连接单飞行请求（客户端串行化一切）。
// 该不变量是冻结面的一部分：协议没有请求 ID，无法在一条连接上并发。
// ===================================================================

// 协议常量定义
const (
	// 魔术字
	MagicNumber uint32 = 0xFBE322A8 // 协议标识符

	// 消息类型。v3 删除了 5 个死类型（HeartbeatPing/Pong 0x0A/0x0B、
	// RecentChange 之外的 Reverify/ReverifyResponse 0x0E/0x0F、
	// Acknowledge 0x07），编号留空洞不复用，避免旧包被误解码
	MsgTypeHandshake            uint16 = 0x0001 // 握手请求/响应
	MsgTypeFileRequest          uint16 = 0x0002 // 文件传输请求
	MsgTypeFileResponse         uint16 = 0x0003 // 文件传输响应
	MsgTypeFileData             uint16 = 0x0004 // 文件数据
	MsgTypeFileComplete         uint16 = 0x0005 // 文件传输完成
	MsgTypeError                uint16 = 0x0006 // 错误消息（结构化，见 ErrorMessage）
	MsgTypeTreeRequest          uint16 = 0x0008 // 目录树请求
	MsgTypeTreeResponse         uint16 = 0x0009 // 目录树响应
	MsgTypeRecentChangeRequest  uint16 = 0x000C // 最近变更请求
	MsgTypeRecentChangeResponse uint16 = 0x000D // 最近变更响应

	// 头部大小
	HeaderSize = 12 // 消息头部大小（魔术字4字节 + 类型2字节 + 长度4字节 + 保留字段2字节）

	// 单条消息体长度上限，防止损坏/恶意的头部导致超大内存分配
	MaxBodyLength = 64 * 1024 * 1024
)

// 错误码（ErrorMessage.Code）。客户端据此区分可重试/永久失败，
// 服务端 handler 用 wireError 构造；未归类的错误一律 ErrCodeInternal
const (
	ErrCodeInternal         uint16 = 0 // 未归类错误，语义等同 v2 的裸字符串
	ErrCodeNotFound         uint16 = 1 // 文件/目录不存在（永久，重试无意义）
	ErrCodePermissionDenied uint16 = 2 // 权限拒绝（永久，上游修复前跳过）
	ErrCodeOutOfRoot        uint16 = 3 // 路径越界/符号链接等策略拒绝（永久）
	ErrCodeTooLarge         uint16 = 4 // 响应超出协议上限（永久）
	ErrCodeVersionMismatch  uint16 = 5 // 协议版本区间无交集（升级前永久）
)

// negotiateVersion 计算会话版本：两端 [min, ver] 区间交集的最高值。
// ok=false 表示交集为空（版本不兼容）
func negotiateVersion(localVer, localMin, peerVer, peerMin uint16) (uint16, bool) {
	agreed := min(localVer, peerVer)
	return agreed, agreed >= localMin && agreed >= peerMin
}

// 消息头定义
type MessageHeader struct {
	Magic        uint32 // 魔术字
	Type         uint16 // 消息类型
	BodyLength   uint32 // 消息体长度
	ReservedWord uint16 // 保留字段
}

// 握手消息（v3：区间协商 + 能力位）
type HandshakeMessage struct {
	Version     uint16 // 支持的最高协议版本
	MinVersion  uint16 // 支持的最低协议版本
	UUID        uint32 // 实例标识
	Role        uint8  // 角色
	FeatureBits uint64 // 能力位（当前恒 0，非零位留给未来能力协商）
}

// 文件请求消息
type FileRequestMessage struct {
	FilePath string // 文件路径
	Offset   uint64 // 起始偏移（断点续传用）
}

// 文件响应消息
type FileResponseMessage struct {
	SessionID [16]byte // 会话ID
	FileSize  uint64   // 文件大小
	FileHash  [32]byte // 文件哈希值
}

// 文件数据消息。数据按流序追加，无逐块偏移（v3 删除了从未被消费的
// Offset 字段）；续传起点由 FileRequest.Offset 一次性确定
type FileDataMessage struct {
	SessionID  [16]byte // 会话ID
	DataLength uint32   // 数据长度
	Data       []byte   // 数据内容
}

// 文件完成消息
type FileCompleteMessage struct {
	SessionID [16]byte // 会话ID
	FileHash  [32]byte // 文件哈希值
}

// ErrorMessage 结构化错误（v3）：错误码 + 关联路径 + 人读消息。
// Path 走线格式路径约定（"/" 分隔），无关联路径时为空
type ErrorMessage struct {
	Code    uint16
	Path    string
	Message string
}

// wireError 服务端 handler 用于携带错误码的 error 实现。
// handleConnection 经 errors.As 提取并编码为 ErrorMessage 下发；
// 未包装的普通 error 落到 ErrCodeInternal
type wireError struct {
	Code    uint16
	Path    string
	Message string
}

func (e *wireError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s: %s", e.Path, e.Message)
	}
	return e.Message
}

// RealityError 客户端收到的服务端错误（结构化）。实现 error，
// 调用方可 errors.As 取 Code 做重试决策（如 PermissionDenied 直接跳过）
type RealityError struct {
	Code    uint16
	Path    string
	Message string
}

func (e *RealityError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("reality error (code %d) %s: %s", e.Code, e.Path, e.Message)
	}
	return fmt.Sprintf("reality error (code %d): %s", e.Code, e.Message)
}

// 树形结构请求消息
type TreeRequestMessage struct {
	RootPath     string // 请求获取的目录树的路径
	ContinueFrom string // 续页游标：上一页最后一个条目的路径；空 = 第一页
}

// 树形结构响应消息。单页最多 treePageMaxEntries 条，
// ContinueFrom 非空表示还有后续页（客户端携带它再次请求）
type TreeResponseMessage struct {
	ContinueFrom string // 非空 = 有下一页，值为本页最后条目路径
	DataLength   uint32 // 数据长度
	Data         []byte // 本页条目的 JSON（[]tree.Node）
}

// 最近变更请求消息。查询区间上界由服务端时钟决定，请求不携带
// （v3 删除了从未被消费的 endTime 字段）
type RecentChangeRequestMessage struct {
	ClientID  uint32 // 客户端标识
	StartTime int64  // 开始时间（秒，服务端时钟系；0=全查窗口）
}

// 最近变更响应消息
type RecentChangeResponseMessage struct {
	ServerID     uint32   // 服务端标识
	CoveredUntil int64    // 本次响应已覆盖到的服务端时刻（秒），客户端据此推进游标
	FullResync   bool     // 变更数超过阈值：列表省略，客户端应全量对账后把游标推进到 CoveredUntil
	Changes      []string // 最近变更的目录列表（FullResync 时为空）
}

// encode 系列函数写入 bytes.Buffer，其 Write 方法在内存不足时 panic 而非返回 error，
// 因此 binary.Write 对 bytes.Buffer 实际上不会返回 error。
// 用 _ = 显式表达"有意忽略"，避免 errcheck 等工具误报。
func encodeHeader(header MessageHeader) []byte {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.BigEndian, header.Magic)
	_ = binary.Write(buf, binary.BigEndian, header.Type)
	_ = binary.Write(buf, binary.BigEndian, header.BodyLength)
	_ = binary.Write(buf, binary.BigEndian, header.ReservedWord)
	return buf.Bytes()
}

func decodeHeader(data []byte) (MessageHeader, error) {
	var header MessageHeader
	buf := bytes.NewReader(data)

	if err := binary.Read(buf, binary.BigEndian, &header.Magic); err != nil {
		return header, err
	}
	if err := binary.Read(buf, binary.BigEndian, &header.Type); err != nil {
		return header, err
	}
	if err := binary.Read(buf, binary.BigEndian, &header.BodyLength); err != nil {
		return header, err
	}
	if err := binary.Read(buf, binary.BigEndian, &header.ReservedWord); err != nil {
		return header, err
	}

	return header, nil
}

func encodeHandshake(msg HandshakeMessage) []byte {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.BigEndian, msg.Version)
	_ = binary.Write(buf, binary.BigEndian, msg.MinVersion)
	_ = binary.Write(buf, binary.BigEndian, msg.UUID)
	_ = binary.Write(buf, binary.BigEndian, msg.Role)
	_ = binary.Write(buf, binary.BigEndian, msg.FeatureBits)
	return buf.Bytes()
}

func decodeHandshake(data []byte) (HandshakeMessage, error) {
	var msg HandshakeMessage
	buf := bytes.NewReader(data)
	if err := binary.Read(buf, binary.BigEndian, &msg.Version); err != nil {
		return msg, err
	}
	if err := binary.Read(buf, binary.BigEndian, &msg.MinVersion); err != nil {
		return msg, err
	}
	if err := binary.Read(buf, binary.BigEndian, &msg.UUID); err != nil {
		return msg, err
	}
	if err := binary.Read(buf, binary.BigEndian, &msg.Role); err != nil {
		return msg, err
	}
	if err := binary.Read(buf, binary.BigEndian, &msg.FeatureBits); err != nil {
		return msg, err
	}
	return msg, nil
}

// writeWirePath 写入 2 字节长度前缀的路径字符串（先转线格式 "/" 分隔）
func writeWirePath(buf *bytes.Buffer, p string) {
	b := []byte(filepath.ToSlash(p))
	_ = binary.Write(buf, binary.BigEndian, uint16(len(b)))
	buf.Write(b)
}

// readWirePath 读取 2 字节长度前缀的字符串并转回本机路径格式
func readWirePath(buf *bytes.Reader) (string, error) {
	var n uint16
	if err := binary.Read(buf, binary.BigEndian, &n); err != nil {
		return "", err
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(buf, b); err != nil {
		return "", err
	}
	return filepath.FromSlash(string(b)), nil
}

func encodeFileRequest(msg FileRequestMessage) []byte {
	buf := new(bytes.Buffer)
	writeWirePath(buf, msg.FilePath)
	_ = binary.Write(buf, binary.BigEndian, msg.Offset)
	return buf.Bytes()
}

func decodeFileRequest(data []byte) (FileRequestMessage, error) {
	var msg FileRequestMessage
	buf := bytes.NewReader(data)
	p, err := readWirePath(buf)
	if err != nil {
		log.Error("Error decoding file request path:", err)
		return msg, err
	}
	msg.FilePath = p
	if err := binary.Read(buf, binary.BigEndian, &msg.Offset); err != nil {
		log.Error("Error decoding file offset:", err)
		return msg, err
	}
	return msg, nil
}

func encodeFileResponse(msg FileResponseMessage) []byte {
	buf := new(bytes.Buffer)
	buf.Write(msg.SessionID[:])
	_ = binary.Write(buf, binary.BigEndian, msg.FileSize)
	buf.Write(msg.FileHash[:])
	return buf.Bytes()
}

func decodeFileResponse(data []byte) (FileResponseMessage, error) {
	var msg FileResponseMessage
	buf := bytes.NewReader(data)

	if _, err := io.ReadFull(buf, msg.SessionID[:]); err != nil {
		log.Error("Error reading session ID:", err)
		return msg, err
	}

	if err := binary.Read(buf, binary.BigEndian, &msg.FileSize); err != nil {
		log.Error("Error decoding file size:", err)
		return msg, err
	}

	if _, err := io.ReadFull(buf, msg.FileHash[:]); err != nil {
		log.Error("Error reading file hash:", err)
		return msg, err
	}

	return msg, nil
}

func encodeFileData(msg FileDataMessage) []byte {
	buf := new(bytes.Buffer)
	buf.Write(msg.SessionID[:])
	_ = binary.Write(buf, binary.BigEndian, msg.DataLength)
	buf.Write(msg.Data)
	return buf.Bytes()
}

func decodeFileData(data []byte) (FileDataMessage, error) {
	var msg FileDataMessage
	buf := bytes.NewReader(data)

	if _, err := io.ReadFull(buf, msg.SessionID[:]); err != nil {
		log.Error("Error reading file data session ID:", err)
		return msg, err
	}

	if err := binary.Read(buf, binary.BigEndian, &msg.DataLength); err != nil {
		log.Error("Error decoding file data length:", err)
		return msg, err
	}

	// 边界校验：声明的数据长度不得超过剩余字节，否则一个小消息声称超大
	// DataLength 会触发数十亿字节的 make() 而 OOM。必须在 make 之前拦截
	if int64(msg.DataLength) > int64(buf.Len()) {
		return msg, fmt.Errorf("file data length %d exceeds remaining %d bytes", msg.DataLength, buf.Len())
	}
	msg.Data = make([]byte, msg.DataLength)
	if _, err := io.ReadFull(buf, msg.Data); err != nil {
		log.Error("Error reading file data:", err)
		return msg, err
	}
	return msg, nil
}

func encodeFileComplete(msg FileCompleteMessage) []byte {
	buf := new(bytes.Buffer)
	buf.Write(msg.SessionID[:])
	buf.Write(msg.FileHash[:])
	return buf.Bytes()
}

func decodeFileComplete(data []byte) (FileCompleteMessage, error) {
	var msg FileCompleteMessage
	buf := bytes.NewReader(data)

	if _, err := io.ReadFull(buf, msg.SessionID[:]); err != nil {
		return msg, fmt.Errorf("error reading file complete session ID: %w", err)
	}
	if _, err := io.ReadFull(buf, msg.FileHash[:]); err != nil {
		return msg, fmt.Errorf("error reading file complete hash: %w", err)
	}
	return msg, nil
}

func encodeErrorMessage(msg ErrorMessage) []byte {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.BigEndian, msg.Code)
	writeWirePath(buf, msg.Path)
	m := []byte(msg.Message)
	_ = binary.Write(buf, binary.BigEndian, uint16(len(m)))
	buf.Write(m)
	return buf.Bytes()
}

func decodeErrorMessage(data []byte) (ErrorMessage, error) {
	var msg ErrorMessage
	buf := bytes.NewReader(data)

	if err := binary.Read(buf, binary.BigEndian, &msg.Code); err != nil {
		log.Error("Error decoding error code:", err)
		return msg, err
	}
	p, err := readWirePath(buf)
	if err != nil {
		log.Error("Error decoding error path:", err)
		return msg, err
	}
	msg.Path = p
	var n uint16
	if err := binary.Read(buf, binary.BigEndian, &n); err != nil {
		log.Error("Error decoding error message length:", err)
		return msg, err
	}
	m := make([]byte, n)
	if _, err := io.ReadFull(buf, m); err != nil {
		log.Error("Error reading error message:", err)
		return msg, err
	}
	msg.Message = string(m)
	return msg, nil
}

func sendMessage(conn net.Conn, msgType uint16, body []byte) error {
	header := MessageHeader{
		Magic:        MagicNumber,
		Type:         msgType,
		BodyLength:   uint32(len(body)),
		ReservedWord: 0,
	}

	headerBytes := encodeHeader(header)
	if _, err := conn.Write(headerBytes); err != nil {
		return err
	}
	if _, err := conn.Write(body); err != nil {
		return err
	}
	log.Debugf("Sent message with type: %d, body length: %d bytes", msgType, len(body))
	return nil
}

func receiveMessage(conn net.Conn) (uint16, []byte, error) {
	headerBytes := make([]byte, HeaderSize)
	if _, err := io.ReadFull(conn, headerBytes); err != nil {
		return 0, nil, fmt.Errorf("%s: %w", conn.RemoteAddr().String(), err)
	}

	header, err := decodeHeader(headerBytes)
	if err != nil {
		remoteAddr := conn.RemoteAddr().String()
		return 0, nil, fmt.Errorf("%s: %w", remoteAddr, err)
	}

	if header.Magic != MagicNumber {
		return 0, nil, fmt.Errorf("invalid magic number in message header from %s, expected %d, got %d", conn.RemoteAddr().String(), MagicNumber, header.Magic)
	}

	if header.BodyLength > MaxBodyLength {
		return 0, nil, fmt.Errorf("message body too large from %s: %d bytes", conn.RemoteAddr().String(), header.BodyLength)
	}

	bodyBytes := make([]byte, header.BodyLength)
	if header.BodyLength > 0 {
		if _, err := io.ReadFull(conn, bodyBytes); err != nil {
			return 0, nil, fmt.Errorf("error reading message body from %s: %w", conn.RemoteAddr().String(), err)
		}
	}

	return header.Type, bodyBytes, nil
}

func encodeTreeRequest(msg TreeRequestMessage) []byte {
	buf := new(bytes.Buffer)
	writeWirePath(buf, msg.RootPath)
	writeWirePath(buf, msg.ContinueFrom)
	return buf.Bytes()
}

func decodeTreeRequest(data []byte) (TreeRequestMessage, error) {
	var msg TreeRequestMessage
	buf := bytes.NewReader(data)

	p, err := readWirePath(buf)
	if err != nil {
		log.Error("Error decoding tree request path:", err)
		return msg, err
	}
	msg.RootPath = p
	c, err := readWirePath(buf)
	if err != nil {
		log.Error("Error decoding tree request cursor:", err)
		return msg, err
	}
	msg.ContinueFrom = c
	return msg, nil
}

func encodeTreeResponse(msg TreeResponseMessage) []byte {
	buf := new(bytes.Buffer)
	writeWirePath(buf, msg.ContinueFrom)
	_ = binary.Write(buf, binary.BigEndian, msg.DataLength)
	buf.Write(msg.Data)
	return buf.Bytes()
}

func decodeTreeResponse(data []byte) (TreeResponseMessage, error) {
	var msg TreeResponseMessage
	buf := bytes.NewReader(data)

	c, err := readWirePath(buf)
	if err != nil {
		log.Error("Error decoding tree response cursor:", err)
		return msg, err
	}
	msg.ContinueFrom = c

	if err := binary.Read(buf, binary.BigEndian, &msg.DataLength); err != nil {
		log.Error("Error decoding tree response data length:", err)
		return msg, err
	}

	if int64(msg.DataLength) > int64(buf.Len()) {
		return msg, fmt.Errorf("tree response data length %d exceeds remaining %d bytes", msg.DataLength, buf.Len())
	}
	msg.Data = make([]byte, msg.DataLength)
	if _, err := io.ReadFull(buf, msg.Data); err != nil {
		log.Error("Error reading tree response data:", err)
		return msg, err
	}

	return msg, nil
}

func encodeRecentChangeRequest(msg RecentChangeRequestMessage) []byte {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.BigEndian, msg.ClientID)
	_ = binary.Write(buf, binary.BigEndian, msg.StartTime)
	return buf.Bytes()
}

func decodeRecentChangeRequest(data []byte) (RecentChangeRequestMessage, error) {
	var msg RecentChangeRequestMessage
	buf := bytes.NewReader(data)

	if err := binary.Read(buf, binary.BigEndian, &msg.ClientID); err != nil {
		log.Error("Error decoding recent change request client ID:", err)
		return msg, err
	}
	if err := binary.Read(buf, binary.BigEndian, &msg.StartTime); err != nil {
		log.Error("Error decoding recent change request startTime:", err)
		return msg, err
	}
	return msg, nil
}

func encodeRecentChangeResponse(msg RecentChangeResponseMessage) []byte {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.BigEndian, msg.ServerID)
	_ = binary.Write(buf, binary.BigEndian, msg.CoveredUntil)
	var fr uint8
	if msg.FullResync {
		fr = 1
	}
	_ = binary.Write(buf, binary.BigEndian, fr)

	changeCount := len(msg.Changes)
	_ = binary.Write(buf, binary.BigEndian, uint32(changeCount))
	for _, change := range msg.Changes {
		changeBytes := []byte(filepath.ToSlash(change))
		_ = binary.Write(buf, binary.BigEndian, uint16(len(changeBytes)))
		buf.Write(changeBytes)
	}

	return buf.Bytes()
}

func decodeRecentChangeResponse(data []byte) (RecentChangeResponseMessage, error) {
	var msg RecentChangeResponseMessage
	buf := bytes.NewReader(data)

	if err := binary.Read(buf, binary.BigEndian, &msg.ServerID); err != nil {
		log.Error("Error decoding recent change response server ID:", err)
		return msg, err
	}
	if err := binary.Read(buf, binary.BigEndian, &msg.CoveredUntil); err != nil {
		log.Error("Error decoding recent change response covered-until:", err)
		return msg, err
	}
	var fr uint8
	if err := binary.Read(buf, binary.BigEndian, &fr); err != nil {
		log.Error("Error decoding recent change response full-resync flag:", err)
		return msg, err
	}
	msg.FullResync = fr != 0

	var changeCount uint32
	if err := binary.Read(buf, binary.BigEndian, &changeCount); err != nil {
		log.Error("Error decoding recent change response change count:", err)
		return msg, err
	}

	// 边界校验：每个条目至少含 2 字节长度前缀，changeCount 超过剩余字节的
	// 一半必然是伪造的——不校验会让 make([]string, 天量) 直接 OOM
	if int64(changeCount) > int64(buf.Len()/2) {
		return msg, fmt.Errorf("change count %d exceeds plausible max for %d remaining bytes", changeCount, buf.Len())
	}
	msg.Changes = make([]string, changeCount)
	for i := uint32(0); i < changeCount; i++ {
		var changeLength uint16
		if err := binary.Read(buf, binary.BigEndian, &changeLength); err != nil {
			log.Error("Error decoding recent change response change length:", err)
			return msg, err
		}
		changeBytes := make([]byte, changeLength)
		if _, err := io.ReadFull(buf, changeBytes); err != nil {
			log.Error("Error reading recent change response change data:", err)
			return msg, err
		}
		msg.Changes[i] = filepath.FromSlash(string(changeBytes))
	}

	return msg, nil
}
