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
	"local-mirror/pkg/utils"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

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
		Mode:     v.Mode,
	}
}

// errQueued processDiffItem 没有当场下载文件，而是把它排进了本目录的批量下载（见 fileQueue），
// 结果稍后由 fetchQueued 交给同一套单项处理
var errQueued = errors.New("queued for batched download")

// fileQueue 一个目录里待批量下载的文件。对端支持 FeatureFetchBatch 时 getDirectory 先处理完
// 目录里的其余各项（删除、建目录、权限、类型互换的移除），把要下载的文件攒在这里一次取回；
// 同一目录的各项路径互不相同，推迟下载不改变任何一项的结果
type fileQueue []DiffResult

// downloadFile 下载一个文件：q 非空时只排进批量下载，否则当场逐个下载
func downloadFile(v DiffResult, fileClient *network.FileClient, b *tree.Batch, q *fileQueue) error {
	if q != nil {
		*q = append(*q, v)
		return errQueued
	}
	return processFileDiff(v, fileClient, b)
}

// processDiffItem handles a single diff item (file or directory).
// q 非空时文件下载排进批量下载并返回 errQueued（见 fileQueue）
func processDiffItem(v DiffResult, fileClient *network.FileClient, b *tree.Batch, q *fileQueue) error {
	switch v.Action {
	case "delete":
		// 默认不删除：仅增量同步，本地多余文件保留。
		// 这样源端异常清空（路径配错、盘没挂上等）不会级联删除下游。
		// 需 --allow-delete 显式开启才做真正的忠实镜像删除
		if !*config.AllowDelete {
			log.Debugf("skipping deletion (--allow-delete off): %s", v.Path)
			return nil
		}
		full, err := safety.SafeResolve(config.StartPath, v.Path)
		if err != nil {
			log.Errorf("refusing to delete out-of-root path: %v", err)
			return nil
		}
		if err := os.RemoveAll(full); err != nil {
			return err
		}
		b.Delete(v.Path)
		return nil

	case "retype":
		// 类型互换（文件↔目录，COR-03）：必须先移除旧类型再建新类型。移除本质是删除，
		// 故仅在 --allow-delete（忠实镜像）下执行；默认增量模式不能删，只能拒绝并一次性
		// 告警后跳过——不做任何操作也不无限重试（本地旧类型原样保留）
		if !*config.AllowDelete {
			warnRetypeOnce(v.Path)
			return nil
		}
		full, err := safety.SafeResolve(config.StartPath, v.Path)
		if err != nil {
			log.Errorf("refusing to retype out-of-root path: %v", err)
			return nil
		}
		// RemoveAll 对文件和目录（含子树）都适用；随后清掉本地树里的旧节点及其子树
		if err := os.RemoveAll(full); err != nil {
			return err
		}
		b.Delete(v.Path)
		// 建新类型：目录直接建，文件走正常下载（上游哈希缺失同 create 分支跳过）
		if v.IsDir {
			return processDirectoryDiff(v, b)
		}
		if v.Hash == "" {
			warnUnreadableOnce(v.Path)
			return nil
		}
		return downloadFile(v, fileClient, b, q)

	case "chmod":
		full, err := safety.SafeResolve(config.StartPath, v.Path)
		if err != nil {
			log.Errorf("refusing to chmod out-of-root path: %v", err)
			return nil
		}
		applyPerm(full, v)
		hash := v.Hash
		if v.IsDir {
			hash = "" // 目录在线格式上的 Hash 是 rollup，不落库
		}
		b.Add(createNodeFromDiff(v, hash), false)
		return nil

	case "create", "modify":
		if v.IsDir {
			return processDirectoryDiff(v, b)
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
		return downloadFile(v, fileClient, b, q)

	default:
		log.Warnf("Unknown action type: %s", v.Action)
		return nil
	}
}

func processDirectoryDiff(v DiffResult, b *tree.Batch) error {
	// v.Path 来自服务端，必须校验拼接后仍在同步根内，防止 ".." 越界建目录
	fullPath, err := safety.SafeResolve(config.StartPath, v.Path)
	if err != nil {
		log.Errorf("refusing to create out-of-root directory: %v", err)
		return nil
	}
	if err := os.MkdirAll(fullPath, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", fullPath, err)
	}
	applyPerm(fullPath, v)

	// 对已存在路径按更新处理，无需先查询
	b.Add(createNodeFromDiff(v, ""), false)
	return nil
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

func warnUnreadableDirOnce(path string) {
	if _, loaded := unreadableWarned.LoadOrStore(path, struct{}{}); !loaded {
		log.Errorf("upstream cannot read directory %s (usually a permission problem); its local copy is left untouched. Sync resumes automatically once fixed upstream", path)
	}
}

// retypeWarned 已提示过的"类型互换但删除未开启"路径。默认增量模式下每轮全量扫描都会
// 重新检出该 retype diff，每路径只提示一次，避免刷屏
var retypeWarned sync.Map

func warnRetypeOnce(path string) {
	if _, loaded := retypeWarned.LoadOrStore(path, struct{}{}); !loaded {
		log.Warnf("%s changed type (file<->directory) upstream; applying it requires removing the old one, which --allow-delete governs. Skipping (local copy kept as-is). Enable --allow-delete for a faithful mirror", path)
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

// checkDiskSpace 磁盘空间预检：free 容不下 v 外加预留余量就返回 ErrDiskFull，由调用方
// 按目录聚合提示
func checkDiskSpace(v DiffResult, free uint64) error {
	if free < v.Size+diskReserve {
		return fmt.Errorf("%w: %s needs %s but only %s is free (reserve %s)",
			appError.ErrDiskFull, v.Path, humanBytes(v.Size), humanBytes(free), humanBytes(diskReserve))
	}
	return nil
}

func processFileDiff(v DiffResult, fileClient *network.FileClient, b *tree.Batch) error {
	// 磁盘空间预检：不够就不发请求、不写分片，返回 ErrDiskFull 由调用方
	// 按目录聚合提示。探测失败（极少见）时放行，交给写入时的兜底识别
	if free, ferr := utils.DiskFree(config.StartPath); ferr == nil {
		if err := checkDiskSpace(v, free); err != nil {
			return err
		}
	}

	hash, err := fileClient.DownloadFile(v.Path, v.Mode)
	if err != nil {
		return downloadFailed(v, err, fileClient)
	}
	fileDownloaded(v, hash, b)
	return nil
}

// downloadFailed 归类一个文件的下载失败，返回交给调用方单项处理的错误（nil = 视为已处理）
func downloadFailed(v DiffResult, err error, conn interface{ ConnectionClose() }) error {
	if errors.Is(err, appError.ErrConnection) {
		conn.ConnectionClose()
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

// fileDownloaded 文件已校验并落到最终位置后的收尾
func fileDownloaded(v DiffResult, hash string, b *tree.Batch) {
	// 保真：把镜像文件的 mtime 设为服务端源文件的 mtime。
	// createNodeFromDiff 随后 stat 磁盘，DB 记录的即这个 mtime，与磁盘一致，
	// 重启校准时不会因时间戳不符而误判为已变化
	applyModTime(v)

	b.Add(createNodeFromDiff(v, hash), true)
	status.RecordFile(v.Path, v.Size)
	log.Infof("File downloaded successfully: %s", v.Path)
}

// fetchBatchMax 一次批量下载请求的文件数上限：请求体保持小巧，连接中断时
// 也只有这一批里没处理到的文件需要重来
const fetchBatchMax = 256

// fileFetcher 批量下载所需的连接能力（*network.FileClient 实现；单测可替换）
type fileFetcher interface {
	FetchFiles(files []network.FetchFile, done func(i int, hash string, err error)) error
	ConnectionClose()
}

// fetchQueued 批量取回本目录排队的文件（FeatureFetchBatch），每批只付一个往返。
// 每个文件的处理与 processFileDiff 逐项一致：磁盘空间预检（每批探一次可用空间，按批内
// 已接纳的大小累计扣减，等同逐个预检时前面的文件已写入）、失败归类、mtime 对齐、入批。
// 每个文件的结果交给 handle（与 processDiffItem 的结果同一套单项处理）；handle 或下载
// 返回连接错误时立即返回该错误，剩余文件留给重连后的重试
func fetchQueued(fileClient fileFetcher, q fileQueue, b *tree.Batch, handle func(DiffResult, error) error) error {
	for len(q) > 0 {
		chunk := q[:min(len(q), fetchBatchMax)]
		q = q[len(chunk):]

		free, ferr := utils.DiskFree(config.StartPath)
		var admitted []DiffResult
		var files []network.FetchFile
		var need uint64
		for _, v := range chunk {
			if ferr == nil {
				if err := checkDiskSpace(v, free-min(need, free)); err != nil {
					if abort := handle(v, err); abort != nil {
						return abort
					}
					continue
				}
			}
			need += v.Size
			admitted = append(admitted, v)
			files = append(files, network.FetchFile{Path: v.Path, Perm: v.Mode})
		}

		var abort error
		err := fileClient.FetchFiles(files, func(i int, hash string, err error) {
			v := admitted[i]
			if err != nil {
				err = downloadFailed(v, err, fileClient)
			} else {
				fileDownloaded(v, hash, b)
			}
			if a := handle(v, err); a != nil && abort == nil {
				abort = a
			}
		})
		if err != nil {
			// 流已不可续读（能归到某个文件的已随它回调过，handle 据此计入单项失败）
			fileClient.ConnectionClose()
			if abort != nil {
				return abort
			}
			return err
		}
		if abort != nil {
			return abort
		}
	}
	return nil
}

// recordChangedDir 中继模式下，把 mirror 引擎应用的变更记入本地变更日志，
// 唤醒下游客户端的长轮询。这比依赖 fsnotify 更精确——中继目录的变更
// 全部来自 mirror 引擎自身，且不受冷目录轮询延迟影响。
// 纯 mirror 模式没有下游，跳过以省去无谓的落库
func recordChangedDir(relPath string, b *tree.Batch) {
	if !config.ServesDownstream() {
		return
	}
	b.ChangedDir(filepath.Dir(relPath))
}

// permWarned 已提示过的"权限设置失败"路径（如 exFAT 等不支持 Unix 权限的文件系统），每路径一次
var permWarned sync.Map

// applyPerm 把上游权限落到本地（尽力而为，同 applyModTime）。失败不阻断同步，
// 数据库仍记上游值，避免每轮全量扫描反复重试
func applyPerm(full string, v DiffResult) {
	if err := tree.ApplyPerm(full, v.IsDir, v.Mode); err != nil {
		if _, loaded := permWarned.LoadOrStore(v.Path, struct{}{}); !loaded {
			log.Warnf("failed to set permissions %o on %s: %v", tree.EffectiveMode(v.IsDir, v.Mode), v.Path, err)
		}
	}
}

// applyModTime 将本地文件的修改时间对齐到服务端源文件
func applyModTime(v DiffResult) {
	if v.ModTime.IsZero() {
		return
	}
	full, err := safety.SafeResolve(config.StartPath, v.Path)
	if err != nil {
		log.Errorf("refusing to set mtime on out-of-root path: %v", err)
		return
	}
	if err := os.Chtimes(full, v.ModTime, v.ModTime); err != nil {
		log.Warnf("Failed to set mtime for %s: %v", v.Path, err)
	}
}
