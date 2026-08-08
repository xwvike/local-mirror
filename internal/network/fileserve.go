package network

import (
	"encoding/json"
	"fmt"
	"io"
	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/internal/safety"
	"local-mirror/internal/status"
	"local-mirror/internal/tree"
	"local-mirror/pkg/utils"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// treePageMaxEntries 目录树响应单页条目上限。每条目 JSON 约 250 字节，
// 两万条约 5 MB，远低于消息体上限（64 MB）；超出的条目经 ContinueFrom
// 续页游标分多次请求，消除超大目录的确定性失败
const treePageMaxEntries = 20000

// localMirrorStateDir 每根状态目录名（cache.db / key / status.json / backups / partial）。
// 文件服务对它独立硬拒（SEC-02 ①），不依赖可配置忽略列表。与 config.forcedIgnores
// 的首项同值，这里独立定义以免 network 反向依赖 config 的未导出常量
const localMirrorStateDir = ".local-mirror"

type session struct {
	ID       [16]byte // 会话ID
	FilePath string   // 文件路径
	FileSize uint64   // 文件大小
	file     *os.File // 文件句柄
	fileHash [32]byte // 文件哈希值
}

// dirSnapshot 一次分页遍历的稳定目录快照（PERF-01）：首页时加载并排序一次，续页复用，
// 避免超大目录每页都全量反序列化 + 排序（N 条目/页 P 条要重复约 N/P 次全量排序）。
// 带 TTL 防陈旧；每客户端只缓存最近一个目录（分页是逐目录走完再走下一个，size=1 足够）
type dirSnapshot struct {
	rootPath string
	nodes    []tree.Node // 已按 Path 升序
	expiry   time.Time
}

// dirSnapshotTTL 分页快照有效期。一次分页遍历远快于此；超时即视为可能陈旧、重新加载。
// 窗口内目录若有增删，个别条目可能漏过一页——与既有的页间容错（变更推送 + 全量扫描）一致
const dirSnapshotTTL = 30 * time.Second

// sortNodesByPath 按 Path 升序原地排序，供分页前建立稳定顺序
func sortNodesByPath(entries []tree.Node) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
}

// pageSortedEntries 从**已按 Path 排序**的条目里取一页。continueFrom 为空取首页，
// 否则从严格大于 continueFrom 的条目开始；next 非空表示还有后续页。
// 页间目录内容可能变化（条目增删导致个别条目漏过一页），由变更推送与
// 全量扫描安全网兜底，与 diff 引擎的既有容错一致
func pageSortedEntries(entries []tree.Node, continueFrom string, limit int) (page []tree.Node, next string) {
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

// pageTreeEntries 排序后取一页（薄封装：非缓存路径与单测用）
func pageTreeEntries(entries []tree.Node, continueFrom string, limit int) (page []tree.Node, next string) {
	sortNodesByPath(entries)
	return pageSortedEntries(entries, continueFrom, limit)
}

// wirePageCopy 返回 page 的线格式副本：清空 ID/ParentID、Path 转 "/"。必须在**副本**上做——
// page 可能是缓存目录快照（dirSnapshot）的子切片，原地改会把 ID 清零、Path 改成 "/" 形式
// 写回缓存，污染后续页的游标比较。Node 无指针字段，浅拷贝即安全（PERF-01 关键正确性点）
func wirePageCopy(page []tree.Node) []tree.Node {
	out := make([]tree.Node, len(page))
	copy(out, page)
	for i := range out {
		out[i].ID = ""
		out[i].ParentID = ""
		// 节点路径随 JSON 进入线格式，统一转为 "/"（见 protocol.go 线格式约定）
		out[i].Path = filepath.ToSlash(out[i].Path)
	}
	return out
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

	// PERF-01：续页复用首页建立的已排序快照，避免超大目录每页都全量加载 + 排序。
	// handleTreeRequest 在该客户端唯一的消息循环 goroutine 内串行执行，dirCache 无需加锁
	c := _client.(*client)
	var entries []tree.Node
	if snap := c.dirCache; snap != nil && treeRequest.ContinueFrom != "" &&
		snap.rootPath == treeRequest.RootPath && time.Now().Before(snap.expiry) {
		entries = snap.nodes
	} else {
		entries, err = tree.GetDirContents(treeRequest.RootPath)
		if err != nil {
			return &wireError{Code: ErrCodeNotFound, Path: treeRequest.RootPath,
				Message: fmt.Sprintf("error getting tree contents: %v", err)}
		}
		sortNodesByPath(entries)
		c.dirCache = &dirSnapshot{rootPath: treeRequest.RootPath, nodes: entries, expiry: time.Now().Add(dirSnapshotTTL)}
	}
	page, next := pageSortedEntries(entries, treeRequest.ContinueFrom, treePageMaxEntries)
	treeData, err := json.Marshal(wirePageCopy(page))
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

// authorizeServeFile 判定一个（已通过词法根检查的）相对路径 rel 是否允许作为文件下发。
// 返回非 nil 即拒绝，统一 ErrCodeNotFound——不区分「忽略/不在树/是目录/不存在」，
// 免得把磁盘上究竟存不存在泄露给探测者。三道闸（SEC-02）：
//
//	① .local-mirror 状态目录独立硬拒，不依赖可配置忽略列表（key/cache.db/status/backups…）；
//	② 命中生效忽略列表即拒（与建树同一个 rel + IsIgnored，语义一致）；
//	③ 必须存在于共享目录树、且为哈希非空的普通文件（目录、软链、哈希失败项都不提供）。
func authorizeServeFile(rel, reportPath string) *wireError {
	notFound := &wireError{Code: ErrCodeNotFound, Path: reportPath, Message: "file not found"}
	if rel == localMirrorStateDir || strings.HasPrefix(rel, localMirrorStateDir+string(filepath.Separator)) {
		return notFound
	}
	if utils.IsIgnored(rel, config.IgnoreFileList) {
		return notFound
	}
	if node, err := tree.GetNodeByPath(rel); err != nil || node == nil || node.IsDir || node.Hash == "" {
		return notFound
	}
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
	rel, relErr := filepath.Rel(config.StartPath, fullPath)
	if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return &wireError{Code: ErrCodeOutOfRoot, Path: fileRequest.FilePath, Message: "illegal file path (escapes sync root)"}
	}
	// 授权闸门（SEC-02）：文件服务只提供「公开目录树里、哈希非空的普通文件」。词法根检查
	// 只保证「没逃出根」，但已握手的对端仍能绕过树枚举、直接点名根内任意路径。策略抽到
	// authorizeServeFile 便于单测；rel 复用上面根检查算出的同一个值，与 tree/IsIgnored 的
	// 键形态（OS 分隔符、根为 "."）一致。
	if werr := authorizeServeFile(rel, fileRequest.FilePath); werr != nil {
		return werr
	}
	// SEC-03：逐级校验请求路径的每一级组件都不是符号链接。只查末段（原 Lstat）挡不住
	// 「中间某级目录是指向根外的符号链接」——后续 Stat/Open 会解引用它，读到同步根之外的
	// 文件（outside→/etc，请求 outside/passwd）。SEC-02 的树成员校验已基本关掉此路（建树跳过
	// 符号链接，故这类路径不在树里），这里作纵深防御 + 收 TOCTOU
	if err := safety.VerifyNoSymlinkComponents(config.StartPath, rel); err != nil {
		return &wireError{Code: ErrCodeOutOfRoot, Path: fileRequest.FilePath, Message: "refusing to serve symlinked path"}
	}
	fileInfo, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &wireError{Code: ErrCodeNotFound, Path: fileRequest.FilePath, Message: "file not found"}
		}
		return fmt.Errorf("error getting file info: %s :%v", fileRequest.FilePath, err)

	} else {
		// 5.4 全局限流：整文件预哈希 + 传输是两次全盘读，256 连接各自触发大文件会把磁盘/CPU
		// 打爆。在此获取全局服务槽（容量远小于连接上限），跨「哈希 → 传输」整段持有、出函数即释放。
		// 阻塞发生在该连接自己的消息循环 goroutine 内——只是排队等槽，不影响其它连接的握手/目录树/
		// 变更长轮询等轻量交互。所有廉价校验（越权/忽略/不在树/不存在/软链）都在获取槽之前完成，
		// 被拒的请求不占槽
		release := acquireFileServeSlot()
		defer release()

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
