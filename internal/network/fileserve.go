package network

import (
	"encoding/hex"
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
	"github.com/zeebo/blake3"
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
	Offset   uint64   // 续传起始偏移（5.2：sendFileData 据此先把前缀 [0,offset) 喂入流式哈希）
	file     *os.File // 文件句柄
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

func (s *fileServer) handleTreeRequest(c *client, bodyBytes []byte) error {
	if err := requireHandshake(c); err != nil {
		return err
	}
	conn := c.Conn
	treeRequest, err := decodeTreeRequest(bodyBytes)
	if err != nil {
		return fmt.Errorf("%w, error decoding tree request: %v", appError.ErrConnection, err)
	}
	clientAddr := conn.RemoteAddr().String()
	log.Infof("Received tree request from %s for path: %s (cursor %q)", clientAddr, treeRequest.RootPath, treeRequest.ContinueFrom)

	// 本端列不出内容的目录：如实回"读不了"，而不是下发缓存/空列表——汇端据此整棵跳过，
	// 不会把它当成空目录删光镜像副本
	if tree.UnderUnreadableDir(treeRequest.RootPath) {
		return &wireError{Code: ErrCodePermissionDenied, Path: treeRequest.RootPath,
			Message: "directory is unreadable on the source; its contents are withheld until it is readable"}
	}

	// PERF-01：续页复用首页建立的已排序快照，避免超大目录每页都全量加载 + 排序。
	// handleTreeRequest 在该客户端唯一的消息循环 goroutine 内串行执行，dirCache 无需加锁
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
	wire := wirePageCopy(page)
	// Merkle rollup 注入：给目录条目填上其子树指纹，供汇端全量扫描据此剪枝未变子树。
	// Hash（对目录原本为空）放不含权限的 rollup，旧版汇端只认它；PermRollup 放含权限的，
	// 新版汇端优先比它。按原始 page[i].Path（本地分隔符）查表，写到已转 "/" 的 wire[i]。
	// 出错则跳过——目录指纹留空，汇端老实全走一遍（兜底）。
	if dirHashes, dhErr := tree.DirHashes(); dhErr == nil {
		permHashes, _ := tree.PermDirHashes()
		for i := range wire {
			if page[i].IsDir {
				wire[i].Hash = dirHashes[page[i].Path]
				wire[i].PermRollup = permHashes[page[i].Path]
			}
		}
	} else {
		log.Warnf("dir rollup hashes unavailable, tree response omits them (client will full-walk): %v", dhErr)
	}
	treeData, err := json.Marshal(wire)
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
//
// 放行时返回命中的树节点（供 handleFileRequest 做 5.2 的 meta-trust：size+mtime 一致就复用
// node.Hash 作为起始哈希，免全量预读）。
func authorizeServeFile(rel, reportPath string) (*tree.Node, *wireError) {
	notFound := &wireError{Code: ErrCodeNotFound, Path: reportPath, Message: "file not found"}
	if rel == localMirrorStateDir || strings.HasPrefix(rel, localMirrorStateDir+string(filepath.Separator)) {
		return nil, notFound
	}
	if utils.IsIgnored(rel, config.IgnoreFileList) {
		return nil, notFound
	}
	node, err := tree.GetNodeByPath(rel)
	if err != nil || node == nil || node.IsDir || node.Hash == "" {
		return nil, notFound
	}
	return node, nil
}

// trustedServeHash 尝试免全量重读地取文件哈希（5.2 meta-trust）：磁盘 size+mtime 与树节点记录
// 一致时，信树里存的哈希，ok=true。ok=false 表示文件自建树后变过（或哈希不可解），调用方回退
// 全量重算。注意：这仅用于 FileResponse 的起始哈希（续传提示）；FileComplete 的权威哈希由
// sendFileData 按实际发送字节流式算出，故此处偶尔陈旧也不会导致完整性误判或活锁
func trustedServeHash(node *tree.Node, fi os.FileInfo) ([32]byte, bool) {
	var h [32]byte
	if node == nil || node.Hash == "" || uint64(fi.Size()) != node.Size || !fi.ModTime().Equal(node.ModTime) {
		return h, false
	}
	b, err := hex.DecodeString(node.Hash)
	if err != nil || len(b) != 32 {
		return h, false
	}
	copy(h[:], b)
	return h, true
}

func (s *fileServer) handleFileRequest(c *client, bodyBytes []byte) error {
	if err := requireHandshake(c); err != nil {
		return err
	}
	conn := c.Conn
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
	node, werr := authorizeServeFile(rel, fileRequest.FilePath)
	if werr != nil {
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
			// 幽灵节点自愈（#3）：该路径已通过 authorizeServeFile（说明它登记在源端树里、
			// 哈希非空），磁盘上却不存在——树登记了给不出的文件。多因短命的生成文件
			// （如 zz_*_test.go）建后即删、watcher 没来得及销账。若不剔除，它会在每轮
			// 全量扫描里被反复 diff→下载→404，那棵子树的 rollup 永不与汇端收敛，
			// 死循环白烧流量。就地从树里剔除，rollup 随即收敛、循环终止；文件若真回来，
			// watcher 会重新登记。DeleteNode 对"本就不在树"的路径是安全 no-op。
			if node != nil {
				if derr := tree.DeleteNode(rel); derr != nil {
					log.Warnf("failed to evict phantom tree node %s (advertised but missing on disk): %v", rel, derr)
				} else {
					log.Infof("evicted phantom tree node %s (advertised but missing on disk)", rel)
				}
			}
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

		// 起始哈希（写入 FileResponse，仅作续传提示）：5.2 meta-trust——磁盘 size+mtime 与树
		// 节点一致就直接复用 node.Hash，免这一遍全量预读；不一致（文件自建树后变过）才回退
		// 全量重算。权威的完整性哈希由 sendFileData 按实际字节流式算出并写入 FileComplete
		fileHash, ok := trustedServeHash(node, fileInfo)
		if !ok {
			var herr error
			fileHash, herr = utils.CalcBlake3(fullPath)
			if herr != nil {
				tree.MarkUnreadable(fullPath)
				if os.IsPermission(herr) {
					return &wireError{Code: ErrCodePermissionDenied, Path: fileRequest.FilePath,
						Message: fmt.Sprintf("error calculating file hash: %v", herr)}
				}
				return fmt.Errorf("error calculating file hash for %s: %v", fileRequest.FilePath, herr)
			}
		}

		file, err := os.Open(fullPath)
		if err != nil {
			// meta-trust 跳过了预读，读不了要在这里登记不可读（原先由 CalcBlake3 失败登记）
			if os.IsPermission(err) {
				tree.MarkUnreadable(fullPath)
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
			Offset:   fileRequest.Offset,
			file:     file,
		}

		c.SessionMap.Store(session.ID, session)

		fileResponse := FileResponseMessage{
			SessionID: sessionBytes,
			FileSize:  uint64(fileInfo.Size()),
			FileHash:  fileHash,
		}
		responseBytes := encodeFileResponse(fileResponse)
		if err := sendMessage(conn, MsgTypeFileResponse, responseBytes); err != nil {
			return fmt.Errorf("%w, error sending file response for %s", appError.ErrConnection, fileRequest.FilePath)
		}
		log.Debugf("Sent file response: session ID: %s, file size: %d bytes", sessionID, fileInfo.Size())
		if err := s.sendFileData(c, session); err != nil {
			return err
		}
		return nil
	}
}

func (s *fileServer) sendFileData(c *client, session *session) error {
	conn := c.Conn
	// session.file 由 handleFileRequest 中的 defer 统一关闭，这里不重复 Close
	defer c.SessionMap.Delete(session.ID)

	rel := strings.Replace(session.FilePath, config.StartPath, ".", 1)

	// 5.2 流式哈希：FileComplete 的权威完整性哈希由「实际发送的字节」流式算出，而不是 serve
	// 入口那个（可能被 meta-trust 复用的）起始哈希。这样即便起始哈希偶尔陈旧，收到的字节与
	// 这里算出的哈希也永远自洽——汇端必匹配，杜绝「陈旧哈希→汇端拒收→重试」的活锁。
	// 续传：文件已 Seek 到 offset，先回读前缀 [0,offset) 喂入 hasher，保证覆盖整文件
	hasher := blake3.New()
	if session.Offset > 0 {
		if _, err := session.file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("error seeking to hash resume prefix of %s", rel)
		}
		if _, err := io.CopyN(hasher, session.file, int64(session.Offset)); err != nil {
			return fmt.Errorf("error hashing resume prefix of %s: %v", rel, err)
		}
		if _, err := session.file.Seek(int64(session.Offset), io.SeekStart); err != nil {
			return fmt.Errorf("error seeking back after prefix hash of %s", rel)
		}
	}

	fileBuf := make([]byte, *config.FileBufferSize)
	var sent uint64
	for {
		n, err := session.file.Read(fileBuf)
		if n > 0 {
			hasher.Write(fileBuf[:n]) // 5.2：边发边喂哈希（在 fileBuf 被下轮 Read 覆盖前）
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
	var completeHash [32]byte
	copy(completeHash[:], hasher.Sum(nil))
	completeMsg := FileCompleteMessage{
		SessionID: session.ID,
		FileHash:  completeHash,
	}

	completeBytes := encodeFileComplete(completeMsg)
	if err := sendMessage(conn, MsgTypeFileComplete, completeBytes); err != nil {
		return fmt.Errorf("%w, error sending file complete for %s", appError.ErrConnection, strings.Replace(session.FilePath, config.StartPath, ".", 1))
	}
	status.RecordFile(strings.Replace(session.FilePath, config.StartPath, ".", 1), session.FileSize)
	log.Infof("Sent file complete message: file path: %s", strings.Replace(session.FilePath, config.StartPath, ".", 1))
	return nil
}
