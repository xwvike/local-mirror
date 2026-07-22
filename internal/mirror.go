package app

import (
	"errors"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/internal/network"
	"local-mirror/internal/safety"
	"local-mirror/internal/status"
	"local-mirror/internal/tree"
	"local-mirror/pkg/stack"
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

// NextLevel 存放待下钻的目录，由 drainNextLevel 消费
var NextLevel = stack.NewStack[DiffResult]()

var taskMutex sync.Mutex // 确保任务串行执行

// lastChangeCursor 记录变更查询已覆盖到的服务端时刻（unix 秒）。
// 该值始终由服务端返回的 CoveredUntil 推进，绝不使用客户端本地时钟，
// 以免客户端时钟快于服务端时漏查中间窗口的变更（服务端 changed_dirs
// 只保留 1 小时）。0 表示"从窗口起点全查"，用作重连/全量扫描后的重置。
// 任务由 taskMutex 保证串行，无需原子操作。
var lastChangeCursor int64

// handleConnectionError wraps connection error handling to reduce duplication
func handleConnectionError(err error, fileClient *network.FileClient) error {
	if errors.Is(err, appError.ErrConnection) {
		fileClient.ConnectionClose()
	}
	return err
}

// createNodeFromDiff creates a tree node from diff info.
// ParentID 必须从本地数据库解析：服务端下发的树已抹掉节点ID，
// 直接使用会导致 children 索引断裂，本地目录永远查不到子节点
func createNodeFromDiff(v DiffResult, hash string) *tree.Node {
	uuid, _ := utils.RandomString(16)
	parentID := ""
	if parent, err := tree.GetNodeByPath(filepath.Dir(v.Path)); err == nil {
		parentID = parent.ID
	} else {
		log.Warnf("Parent node not found for %s: %v", v.Path, err)
	}
	// ModTime 必须取磁盘上的真实值：启动校准按 size+mtime 判断哈希可否复用，
	// 记下载时刻会导致重启后所有文件都被误判为已变化而重算哈希
	modTime := time.Now()
	if info, err := os.Stat(filepath.Join(config.StartPath, v.Path)); err == nil {
		modTime = info.ModTime()
	}
	return &tree.Node{
		ID:       uuid,
		Path:     v.Path,
		Name:     v.Name,
		ParentID: parentID,
		IsDir:    v.IsDir,
		Size:     v.Size,
		ModTime:  modTime,
		Hash:     hash,
		Depth:    strings.Count(v.Path, string(filepath.Separator)),
	}
}

func executeTaskWithClient(taskName string, fileClient *network.FileClient, taskFunc func(*network.FileClient) error) error {
	if fileClient.State == network.Deprecated {
		return fmt.Errorf("client is deprecated")
	}

	taskMutex.Lock()
	defer taskMutex.Unlock()

	log.Infof("task started: %s", taskName)
	startTime := time.Now()

	if err := taskFunc(fileClient); err != nil {
		log.Errorf("task failed %s: %v", taskName, err)
		if errors.Is(err, appError.ErrConnection) {
			return fmt.Errorf("client became deprecated during task: %w", err)
		}
	}

	duration := time.Since(startTime)
	log.Infof("task done: %s, took %v", taskName, duration)
	return nil
}

// processDiffItem handles a single diff item (file or directory)
func processDiffItem(v DiffResult, fileClient *network.FileClient) error {
	switch v.Action {
	case "delete":
		// 默认不删除：仅增量同步，本地多余文件保留。
		// 这样源端异常清空（路径配错、盘没挂上等）不会级联删除下游。
		// 需 --allow-delete 显式开启才做真正的忠实镜像删除
		if !*config.AllowDelete {
			log.Debugf("skipping deletion (--allow-delete off): %s", v.Path)
			return nil
		}
		full, err := safety.SafeJoin(config.StartPath, v.Path)
		if err != nil {
			log.Errorf("refusing to delete out-of-root path: %v", err)
			return nil
		}
		if err := os.RemoveAll(full); err == nil {
			tree.DeleteNode(v.Path)
			return nil
		} else {
			return err
		}

	case "create", "modify":
		if v.IsDir {
			return processDirectoryDiff(v)
		}
		// 上游哈希缺失 = 服务端自己都读不了这个文件（扫描/监听时哈希失败，
		// 典型是权限问题），下载注定失败——确定性跳过并明确告知，而不是发一个
		// 注定失败的请求。节点仍在上游树里，本地已有副本因此不会被
		// --allow-delete 误删；上游修复权限后（watcher 对 Chmod 事件重算哈希）
		// 自动恢复同步
		if v.Hash == "" {
			warnUnreadableOnce(v.Path)
			return nil
		}
		return processFileDiff(v, fileClient)

	default:
		log.Warnf("Unknown action type: %s", v.Action)
		return nil
	}
}

func processDirectoryDiff(v DiffResult) error {
	// v.Path 来自服务端，必须校验拼接后仍在同步根内，防止 ".." 越界建目录
	fullPath, err := safety.SafeJoin(config.StartPath, v.Path)
	if err != nil {
		log.Errorf("refusing to create out-of-root directory: %v", err)
		return nil
	}
	if err := os.MkdirAll(fullPath, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", fullPath, err)
	}

	// AddNodes 对已存在路径按更新处理，无需先查询
	node := createNodeFromDiff(v, "")
	return tree.AddNodes([]*tree.Node{node})
}

// diskReserve 磁盘空间预留：可用空间必须容得下目标文件之外再留出这个余量，
// 否则跳过下载。把盘写到全满会连累状态库（bbolt）与日志的写入一起失败，
// 且中途 ENOSPC 只能断连重连（协议无中止机制），代价远高于预检跳过
const diskReserve uint64 = 64 << 20 // 64 MB

// unreadableWarned 已提示过的"上游不可读"路径。每路径只提示一次，
// 避免每轮变更推送/全量扫描都重复刷同一批文件
var unreadableWarned sync.Map

func warnUnreadableOnce(path string) {
	if _, loaded := unreadableWarned.LoadOrStore(path, struct{}{}); !loaded {
		log.Errorf("upstream cannot read %s (server failed to hash it, usually a permission problem); skipping. Sync resumes automatically once fixed upstream", path)
	}
}

func humanBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	}
}

func processFileDiff(v DiffResult, fileClient *network.FileClient) error {
	// 磁盘空间预检：不够就不发请求、不写分片，返回 ErrDiskFull 由调用方
	// 按目录聚合提示。探测失败（极少见）时放行，交给写入时的兜底识别
	if free, ferr := utils.DiskFree(config.StartPath); ferr == nil && free < v.Size+diskReserve {
		return fmt.Errorf("%w: %s needs %s but only %s is free (reserve %s)",
			appError.ErrDiskFull, v.Path, humanBytes(v.Size), humanBytes(free), humanBytes(diskReserve))
	}

	hash, err := fileClient.DownloadFile(v.Path)
	if err != nil {
		if errors.Is(err, appError.ErrConnection) {
			fileClient.ConnectionClose()
			return err
		}
		// 权限拒绝是永久失败（上游修好前重试恒败）：按"上游不可读"处理，
		// 每路径提示一次后本轮直接跳过，不再反复请求刷日志。
		// 上游恢复可读后由 watcher 补哈希、变更推送自动恢复同步
		var re *network.RealityError
		if errors.As(err, &re) && re.Code == network.ErrCodePermissionDenied {
			warnUnreadableOnce(v.Path)
			return nil
		}
		status.RecordError()
		log.Errorf("Error downloading file %s: %v", v.Path, err)
		return err
	}

	// 保真：把镜像文件的 mtime 设为服务端源文件的 mtime。
	// createNodeFromDiff 随后 stat 磁盘，DB 记录的即这个 mtime，与磁盘一致，
	// 重启校准时不会因时间戳不符而误判为已变化
	applyModTime(v)

	fileNode := createNodeFromDiff(v, hash)
	if err := tree.AddNodes([]*tree.Node{fileNode}); err != nil {
		return err
	}
	status.RecordFile(v.Path, v.Size)
	log.Infof("File downloaded successfully: %s", v.Path)
	return nil
}

// recordChangedDir 中继模式下，把 mirror 引擎应用的变更记入本地变更日志，
// 唤醒下游客户端的长轮询。这比依赖 fsnotify 更精确——中继目录的变更
// 全部来自 mirror 引擎自身，且不受冷目录轮询延迟影响。
// 纯 mirror 模式没有下游，跳过以省去无谓的落库
func recordChangedDir(relPath string) {
	if !config.ServesDownstream() {
		return
	}
	tree.AddRecentChangedDir(filepath.Dir(relPath))
}

// applyModTime 将本地文件的修改时间对齐到服务端源文件
func applyModTime(v DiffResult) {
	if v.ModTime.IsZero() {
		return
	}
	full, err := safety.SafeJoin(config.StartPath, v.Path)
	if err != nil {
		log.Errorf("refusing to set mtime on out-of-root path: %v", err)
		return
	}
	if err := os.Chtimes(full, v.ModTime, v.ModTime); err != nil {
		log.Warnf("Failed to set mtime for %s: %v", v.Path, err)
	}
}

// maxItemRetries 单个 diff 项（通常是文件）连续触发连接错误后拉黑的次数上限。
// 目录内某一项持续失败（权限、磁盘满等本地错误）不应该无限期拖累同目录
// 其余正常文件的同步——拉黑后该项在本轮内不再尝试，其余项照常处理。
//
// 不变量：必须满足 maxItemRetries <= maxDirRetries。问题文件的前
// maxItemRetries 次失败会累积目录级失败计数（拉黑发生后才清零），
// 若本值更大，目录会在文件被拉黑之前先耗尽重试预算被整体放弃。
const maxItemRetries = 3

// getDirectory 同步单个目录：拉取服务端目录列表、执行差异处理，
// 并把需要继续下钻的子目录压入 NextLevel。
// recurseAll 为 true 时所有子目录都下钻（全量扫描的安全网语义）；
// 为 false 时只下钻本次新建/变更的子目录。
// itemFailures/blacklist 是跨多次目录重试共享的状态（调用方持有），用于把
// 持续失败的具体路径隔离掉，使目录内其余正常项不受拖累。
func getDirectory(fileClient *network.FileClient, path string, recurseAll bool, itemFailures map[string]int, blacklist map[string]bool) error {
	// 客户端忽略：命中忽略列表的目录整体跳过（变更追踪可能推来
	// 忽略目录内的深层路径，连目录列表请求都不必发）
	if utils.IsIgnored(path, config.IgnoreFileList) {
		log.Debugf("skipping ignored directory: %s", path)
		return nil
	}
	// 树响应按页下发并在客户端内聚合（超大目录不再撞消息体上限），
	// 返回的节点路径已是本机分隔符格式
	realityNodes, err := fileClient.GetRealityTree(path)
	if err != nil {
		return handleConnectionError(err, fileClient)
	}

	diffs, err := Diff(realityNodes, path)
	if err != nil {
		return fmt.Errorf("error analyzing diff for path %s: %w", path, err)
	}

	// 客户端忽略：命中项从 diff 中整体剔除——create/modify 不下载、
	// delete 不删除、也不参与后面的重命名配对。服务端未忽略而客户端
	// 忽略的条目由此对同步完全隐形（本地已有的副本也不会被碰）
	diffs = filterIgnoredDiffs(diffs)

	// 保真：就地重命名的文件走本地 rename，免整文件重新下载
	diffs = detectRenames(diffs)

	log.Infof("Diff count for %s: %d", path, len(diffs))
	diffDirs := make(map[string]bool)
	diskFullSkipped := 0
	for _, v := range diffs {
		if blacklist[v.Path] {
			// 已确认持续失败，本轮不再尝试，让其余正常项能被处理到
			continue
		}
		if err := processDiffItem(v, fileClient); err != nil {
			// 磁盘空间不足：跳过该文件继续处理其余项（小文件可能仍装得下），
			// 目录处理完后聚合成一条提示，避免逐文件刷屏
			if errors.Is(err, appError.ErrDiskFull) {
				diskFullSkipped++
				log.Debugf("skipped for low disk space: %v", err)
				continue
			}
			// 连接断了：无论是否拉黑，这次调用都不能继续复用这个连接处理
			// 剩余项，必须整体返回交给上层重连后重试；其他错误跳过单项继续
			if errors.Is(err, appError.ErrConnection) {
				itemFailures[v.Path]++
				if itemFailures[v.Path] > maxItemRetries {
					blacklist[v.Path] = true
					log.Errorf("%s failed %d times in a row, giving it up for this round (other files unaffected)", v.Path, itemFailures[v.Path]-1)
				}
				return err
			}
			log.Errorf("Error processing diff item %v: %v", v, err)
			continue
		}
		recordChangedDir(v.Path)
		if v.IsDir && v.Action != "delete" {
			diffDirs[v.Path] = true
			NextLevel.Push(v)
		}
	}
	if diskFullSkipped > 0 {
		free, _ := utils.DiskFree(config.StartPath)
		log.Errorf("directory %s: %d files skipped for low disk space (%s free, %s reserved); they will catch up automatically once space is freed",
			path, diskFullSkipped, humanBytes(free), humanBytes(diskReserve))
	}

	if recurseAll {
		for _, node := range realityNodes {
			if node.IsDir && utils.IsIgnored(node.Path, config.IgnoreFileList) {
				// 忽略目录不下钻（服务端可能没忽略它，树里存在）
				continue
			}
			if node.IsDir && !diffDirs[node.Path] {
				NextLevel.Push(DiffResult{
					Path:   node.Path,
					IsDir:  true,
					Action: "modify",
					Name:   node.Name,
					Size:   node.Size,
				})
			}
		}
	}
	return nil
}

// filterIgnoredDiffs 剔除命中忽略列表的 diff 项。
// 客户端忽略语义：不下载（create/modify）、不删除（delete）——
// 即便服务端树里有该条目，也当它不存在；本地磁盘上的同名内容原样保留
func filterIgnoredDiffs(diffs []DiffResult) []DiffResult {
	kept := diffs[:0]
	for _, d := range diffs {
		if utils.IsIgnored(d.Path, config.IgnoreFileList) {
			log.Debugf("ignoring diff item (%s): %s", d.Action, d.Path)
			continue
		}
		kept = append(kept, d)
	}
	return kept
}

// detectRenames 在单个目录的 diff 内识别"就地重命名"：一个 delete 与一个
// create 若指向哈希相同的文件（内容未变、仅换名），直接本地 rename，
// 避免整文件重新下载。返回消化掉重命名对之后剩余的 diff。
// 仅处理同目录内的文件（跨目录移动分属不同目录的 diff，无法在此配对）。
func detectRenames(diffs []DiffResult) []DiffResult {
	// 按哈希索引待删除的文件（每个哈希取第一个）
	delIdxByHash := make(map[string]int)
	for i, d := range diffs {
		if d.Action == "delete" && !d.IsDir && d.Hash != "" {
			if _, exists := delIdxByHash[d.Hash]; !exists {
				delIdxByHash[d.Hash] = i
			}
		}
	}
	if len(delIdxByHash) == 0 {
		return diffs
	}

	handled := make(map[int]bool)
	for i, d := range diffs {
		if d.Action != "create" || d.IsDir || d.Hash == "" {
			continue
		}
		di, ok := delIdxByHash[d.Hash]
		if !ok || handled[di] || diffs[di].Path == d.Path {
			continue
		}
		if err := applyRename(diffs[di], d); err != nil {
			log.Warnf("rename %s -> %s failed, falling back to download: %v", diffs[di].Path, d.Path, err)
			continue
		}
		handled[i] = true
		handled[di] = true
		log.Infof("move detected: %s -> %s (local rename, no download)", diffs[di].Path, d.Path)
	}
	if len(handled) == 0 {
		return diffs
	}

	remaining := make([]DiffResult, 0, len(diffs)-len(handled))
	for i, d := range diffs {
		if !handled[i] {
			remaining = append(remaining, d)
		}
	}
	return remaining
}

// applyRename 执行一次就地重命名：本地移动文件、对齐 mtime、更新数据库
func applyRename(oldDiff, newDiff DiffResult) error {
	oldFull, err := safety.SafeJoin(config.StartPath, oldDiff.Path)
	if err != nil {
		log.Errorf("refusing to rename from out-of-root path: %v", err)
		return nil
	}
	newFull, err := safety.SafeJoin(config.StartPath, newDiff.Path)
	if err != nil {
		log.Errorf("refusing to rename to out-of-root path: %v", err)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(newFull), 0755); err != nil {
		return err
	}
	if err := os.Rename(oldFull, newFull); err != nil {
		return err
	}
	applyModTime(newDiff)
	if err := tree.DeleteNode(oldDiff.Path); err != nil {
		return err
	}
	if err := tree.AddNodes([]*tree.Node{createNodeFromDiff(newDiff, newDiff.Hash)}); err != nil {
		return err
	}
	// 重命名影响新旧两个父目录
	recordChangedDir(oldDiff.Path)
	recordChangedDir(newDiff.Path)
	return nil
}

// maxDirRetries 单个目录连续失败后放弃的次数上限。
// 若失败原因是持续性的本地错误（权限、磁盘满等），每次重连后立即重试会
// 无限速循环——之前一次 ulimit 复现里，1 秒内触发了 1300+ 次重连。
// 必须既限制重试次数、又在重试前退避，而不是任由其中一种机制单独兜底
const maxDirRetries = 3

// drainNextLevel 逐层消费 NextLevel 中的目录，连接错误时重连并重试当前目录。
// 同一目录连续失败达到上限后放弃该目录（记录错误），避免持续性本地错误
// 导致无退避的重连风暴
func drainNextLevel(fileClient *network.FileClient, recurseAll bool) error {
	retries := make(map[string]int)
	// itemFailures/blacklist 跨目录的多次重试持续存在：目录内某个具体文件
	// 反复失败会被拉黑（见 getDirectory），使同目录其余正常文件不被拖累
	itemFailures := make(map[string]int)
	blacklist := make(map[string]bool)

	for NextLevel.Size() > 0 {
		v, _ := NextLevel.Pop()
		log.Debugf("Processing next level item: %v 【%d】remaining", v, NextLevel.Size())

		if !v.IsDir {
			log.Error("Unexpected file type in NextLevel stack, expected directory but got file:", v.Path)
			continue
		}

		blacklistBefore := len(blacklist)
		err := getDirectory(fileClient, v.Path, recurseAll, itemFailures, blacklist)
		if err == nil {
			continue
		}

		log.Errorf("Error processing directory %s: %v", v.Path, err)
		if errors.Is(err, appError.ErrConnection) {
			if len(blacklist) > blacklistBefore {
				// 本轮拉黑了一个新的问题文件，说明在收敛（diff 下一轮会变小），
				// 不计入目录失败次数，避免"目录内有多个问题文件"时，
				// 目录级重试预算在文件逐个被拉黑之前就被耗尽
				retries[v.Path] = 0
			} else {
				retries[v.Path]++
			}
			if retries[v.Path] > maxDirRetries {
				log.Errorf("directory %s failed %d times in a row, giving up this round", v.Path, retries[v.Path]-1)
				continue
			}
			if reconnectErr := fileClient.Reconnect(); reconnectErr != nil {
				return err
			}
			// 退避后再重试：给持续性错误留出恢复窗口，也避免忙循环
			time.Sleep(time.Duration(retries[v.Path]) * time.Second)
			NextLevel.Push(v)
		}
	}
	return nil
}

// ensureConnected makes sure we have a valid connection
func ensureConnected() (*network.FileClient, error) {
	fileClient, err := InitConn()
	if err != nil {
		fileClient.ConnectionClose()
		// 保留探测的具体失败原因（如加密口令不一致），方便用户定位
		return fileClient, err
	}

	if fileClient.State == network.Online {
		return fileClient, nil
	}

	return fileClient, fmt.Errorf("failed to establish connection")
}

func Mirror() {
	log.Debug("step 3 >> start file client")
	baseDelay := 5 * time.Second
	maxDelay := 60 * time.Second
	currentDelay := baseDelay
	for {
		fileClient, err := ensureConnected()
		if err != nil {
			log.Error("Failed to connect: ", err)
			time.Sleep(currentDelay)
			currentDelay = time.Duration(float64(currentDelay) * 1.5)
			currentDelay = min(currentDelay, maxDelay)
			continue
		}
		currentDelay = baseDelay
		status.SessionUp(fmt.Sprintf("connected to %s", fileClient.RealityAddr))
		err = runMirrorTasks(fileClient)
		status.SessionDown()
		if err != nil {
			status.RecordError()
			log.Errorf("Error running mirror tasks: %v", err)
			fileClient.ConnectionClose()
			time.Sleep(5 * time.Second)
			continue
		}
	}
}

// MirrorListen 汇监听格（--receive --listen，四象限）：不拨出，在
// ServerListener 上等源端拨入，每条入站连接跑一轮完整镜像会话。
// 协议报文与谁拨号无关——汇仍先说话（accept 后立即发握手）。
// 单上游串行服务：会话期间不 accept，多余拨入留在内核 backlog，
// 当前会话结束后自然轮到（拨号方有自己的重试退避）。
// 入站连接断开后不可重拨（主动权在源端），回到 accept 等下一条
func MirrorListen() {
	log.Debug("step 3 >> start sink listener")
	if ServerListener == nil {
		log.Fatal("server listener not initialized")
	}
	log.Infof("Sink listening on %s, waiting for the source to dial in", ServerListener.Addr())
	for {
		conn, err := ServerListener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Error("Error accepting inbound source:", err)
			continue
		}
		prepared, err := network.PrepareInboundConn(conn)
		if err != nil {
			log.Warnf("Rejecting inbound %s: %v", conn.RemoteAddr(), err)
			continue
		}
		fileClient := network.NewFileClientFromConn(prepared)
		if err := fileClient.Handshake(); err != nil {
			log.Warnf("Inbound source %s handshake failed: %v", conn.RemoteAddr(), err)
			fileClient.ConnectionClose()
			continue
		}
		log.Infof("Source dialed in from %s, mirror session starting", conn.RemoteAddr())
		status.SessionUp(fmt.Sprintf("source dialed in from %s", conn.RemoteAddr()))
		if err := runMirrorTasks(fileClient); err != nil {
			status.RecordError()
			log.Errorf("Mirror session over inbound transport ended: %v", err)
		}
		status.SessionDown()
		fileClient.ConnectionClose()
	}
}

// sleepDetectThreshold 长轮询往返最长约 LongPollReadTimeout（60s），
// 墙钟跳变远超此值即判定刚从系统休眠中醒来
const sleepDetectThreshold = 3 * time.Minute

func runMirrorTasks(fileClient *network.FileClient) error {
	// 连接后先全量对账；重连（含休眠后 socket 断开）都会重新走到这里
	if err := executeTaskWithClient("initial full scan", fileClient, fullScan); err != nil {
		return err
	}

	// 有了实时推送，全量扫描退化为低频安全网
	fullScanInterval := time.Duration(*config.CoolDown) * time.Second
	lastFullScan := time.Now()

	for {
		// 长轮询：阻塞等待服务端推送变更（无变更时约 LongPollHold 后返回空）。
		// 空闲时客户端就阻塞在这一个 socket 读上，零轮询、零额外唤醒
		beforePoll := time.Now()
		if err := executeTaskWithClient("change tracking", fileClient, TrackingChanges); err != nil {
			return err
		}

		// 休眠感知：长轮询最多挂 ~60s，墙钟却跳了远超此值 → 刚从休眠醒来。
		// 服务端 changed_dirs 只保留 1 小时，睡久了增量窗口不可信，强制全量对账
		if elapsed := time.Since(beforePoll); elapsed > sleepDetectThreshold {
			log.Warnf("long sleep detected (%v), forcing a full reconciliation", elapsed.Round(time.Second))
			if err := executeTaskWithClient("post-wake full scan", fileClient, fullScan); err != nil {
				return err
			}
			lastFullScan = time.Now()
			continue
		}

		// 低频全量扫描安全网，兜住推送链路任何潜在遗漏
		if time.Since(lastFullScan) >= fullScanInterval {
			if err := executeTaskWithClient("full scan", fileClient, fullScan); err != nil {
				return err
			}
			lastFullScan = time.Now()
		}
	}
}

func fullScan(fileClient *network.FileClient) error {
	startTime := time.Now()

	NextLevel.Clear()
	NextLevel.Push(DiffResult{
		Path:   ".",
		IsDir:  true,
		Action: "create",
		Name:   "root",
	})

	if err := drainNextLevel(fileClient, true); err != nil {
		return err
	}

	// 不用客户端时钟设置游标（会因时钟偏差漏查）。全量扫描后把游标重置为 0，
	// 下一次变更追踪以 [0, 服务端now] 全查一次窗口（此时多为已同步的空 diff），
	// 并从服务端返回的 CoveredUntil 重新确立游标，之后全程服务端时钟。
	// 这也顺带覆盖了扫描期间发生的变更，不会遗漏。
	lastChangeCursor = 0

	log.Infof("Full scan completed, total time taken: %v", time.Since(startTime))
	return nil
}

func TrackingChanges(fileClient *network.FileClient) error {
	change, coveredUntil, fullResync, err := fileClient.GetTreeChange(lastChangeCursor)
	if err != nil {
		return handleConnectionError(err, fileClient)
	}

	if fullResync {
		// 服务端本区间变更数超阈值，列表被省略：全量对账一次。
		// 注意 fullScan 会把游标归 0——若沿用，下一轮又会查到同一批超限
		// 变更再触发全量，活锁到日志窗口滑过为止。这里覆盖为本次响应的
		// CoveredUntil：全量扫描发生在响应之后，该时刻前的状态已被扫描覆盖
		log.Warnf("server reports too many changed directories in the window, falling back to a full reconciliation")
		if err := fullScan(fileClient); err != nil {
			return err
		}
		lastChangeCursor = coveredUntil
		return nil
	}

	if len(change) == 0 {
		// 长轮询保活返回，无变更；推进游标到服务端已覆盖时刻
		lastChangeCursor = coveredUntil
		return nil
	}
	allPaths := extractMinimalPathsFromChanges(change)
	NextLevel.Clear()
	// 本次变更批次内共享的失败隔离状态；不跨多次 TrackingChanges 调用持续，
	// 一个文件持续失败时下次心跳周期会重新尝试（成本很低，且能自愈）
	itemFailures := make(map[string]int)
	blacklist := make(map[string]bool)
	for _, v := range allPaths {
		log.Infof("Processing change: %v", v)
		err := getDirectory(fileClient, v, false, itemFailures, blacklist)
		if err == nil {
			continue
		}
		log.Errorf("Error processing directory %s: %v", v, err)
		if errors.Is(err, appError.ErrConnection) {
			if reconnectErr := fileClient.Reconnect(); reconnectErr != nil {
				return err
			}
		}
	}
	// 变更中新出现的子目录需要继续下钻，否则要等下次全量扫描才能同步到
	if err := drainNextLevel(fileClient, false); err != nil {
		return err
	}
	// 游标推进到服务端本次已覆盖的时刻，不重叠不遗漏
	lastChangeCursor = coveredUntil
	return nil
}

func extractMinimalPathsFromChanges(changePaths []string) []string {
	var neededPaths []string
	processedPaths := make(map[string]bool)

	for _, path := range changePaths {
		if path == "" || path == "/" {
			continue
		}

		// 检查路径的父目录链，只添加不存在的父目录
		pathsToAdd := []string{path} // 总是包含变更的路径本身

		currentPath := filepath.Dir(path)
		for currentPath != "." && currentPath != "/" && currentPath != "" {
			// 检查父目录是否存在于本地
			exists, err := tree.HasPath(currentPath)
			if err != nil {
				log.Errorf("Error checking path %s: %v", currentPath, err)
				break
			}

			if !exists {
				pathsToAdd = append([]string{currentPath}, pathsToAdd...) // 前置插入
				currentPath = filepath.Dir(currentPath)
			} else {
				// 父目录存在，无需继续向上查找
				break
			}
		}

		// 添加到需要处理的路径列表
		for _, p := range pathsToAdd {
			if !processedPaths[p] {
				neededPaths = append(neededPaths, p)
				processedPaths[p] = true
			}
		}
	}

	// 按深度排序
	sort.Slice(neededPaths, func(i, j int) bool {
		depthI := strings.Count(neededPaths[i], string(filepath.Separator))
		depthJ := strings.Count(neededPaths[j], string(filepath.Separator))
		if depthI == depthJ {
			return neededPaths[i] < neededPaths[j]
		}
		return depthI < depthJ
	})

	return neededPaths
}
