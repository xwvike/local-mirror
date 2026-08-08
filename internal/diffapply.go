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
	}
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
		full, err := safety.SafeResolve(config.StartPath, v.Path)
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
		if err := tree.DeleteNode(v.Path); err != nil {
			return err
		}
		// 建新类型：目录直接建，文件走正常下载（上游哈希缺失同 create 分支跳过）
		if v.IsDir {
			return processDirectoryDiff(v)
		}
		if v.Hash == "" {
			warnUnreadableOnce(v.Path)
			return nil
		}
		return processFileDiff(v, fileClient)

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
	fullPath, err := safety.SafeResolve(config.StartPath, v.Path)
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
	full, err := safety.SafeResolve(config.StartPath, v.Path)
	if err != nil {
		log.Errorf("refusing to set mtime on out-of-root path: %v", err)
		return
	}
	if err := os.Chtimes(full, v.ModTime, v.ModTime); err != nil {
		log.Warnf("Failed to set mtime for %s: %v", v.Path, err)
	}
}
