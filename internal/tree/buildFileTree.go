package tree

import (
	"fmt"
	"io/fs"
	"local-mirror/config"
	"local-mirror/pkg/utils"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

var (
	recentChangedDirs    []string
	mu                   sync.Mutex
	addChangeTimer       *time.Timer
	addChangeTimerActive bool
)

// 变更广播：目录变更落库后唤醒服务端挂起的长轮询请求。
// 用"关闭并替换 channel"实现一对多广播——等待方每次 select 前
// 通过 ChangeSignal() 取当前 channel，落库时 close 唤醒所有等待者。
var (
	changeSignalMu sync.Mutex
	changeSignalCh = make(chan struct{})
)

// ChangeSignal 返回一个在下一次变更落库时会被关闭的 channel。
// 调用方必须在每次查询前重新获取，才能捕捉到查询与等待之间发生的变更。
func ChangeSignal() <-chan struct{} {
	changeSignalMu.Lock()
	defer changeSignalMu.Unlock()
	return changeSignalCh
}

func broadcastChange() {
	changeSignalMu.Lock()
	defer changeSignalMu.Unlock()
	close(changeSignalCh)
	changeSignalCh = make(chan struct{})
}

// AddRecentChangedDir 记录发生变更的目录，2 秒节流后批量落库。
// 注意是节流不是防抖：持续的事件流不会不断推迟落库，
// 否则客户端按时间窗查询时会错过这些迟到的记录。
func AddRecentChangedDir(dirPath string) {
	mu.Lock()
	defer mu.Unlock()

	if !slices.Contains(recentChangedDirs, dirPath) {
		recentChangedDirs = append(recentChangedDirs, dirPath)
	}

	if addChangeTimerActive {
		return
	}
	addChangeTimer = time.AfterFunc(2*time.Second, func() {
		// 回调运行在独立 goroutine，取快照后再落库，避免与并发的 Add 竞争
		mu.Lock()
		batch := recentChangedDirs
		recentChangedDirs = nil
		addChangeTimerActive = false
		mu.Unlock()

		if len(batch) == 0 {
			return
		}
		if err := addChangedDir(batch); err != nil {
			log.Error("Failed to add changed directories:", err)
			return
		}
		// 落库成功后才广播：客户端查询的是持久化的 changed_dirs，
		// 广播早于落库会让被唤醒的查询扑空
		broadcastChange()
	})
	addChangeTimerActive = true
}

// BuildFileTree 遍历磁盘构建目录树并写入数据库。
// 若数据库中已有上次运行的缓存（见 InitDB），则按校准模式运行：
// 复用未变化文件（size+mtime 一致）的哈希，只重算变化的文件，
// 并清理磁盘上已不存在的失效节点
func BuildFileTree(path string) error {
	startTime := time.Now().UnixMilli()
	log.Info("start build file tree with concurrent WalkDir from path:", path)

	// 获取根节点信息
	rootInfo, err := os.Stat(path)
	if err != nil {
		log.Error("Failed to get root node info, path may not exist:", path)
		return err
	}
	if !rootInfo.IsDir() {
		log.Error("The specified path is not a directory:", path)
		return err
	}

	// 上次运行留下的节点缓存（首次运行为空表）
	existing, err := LoadAllNodesByPath()
	if err != nil {
		return fmt.Errorf("failed to load cached nodes: %w", err)
	}

	// 创建根节点。已有缓存时必须复用旧 ID：
	// children 索引以节点 ID 为键，换新 ID 会切断所有既有的父子关系
	rootID := ""
	if old, ok := existing["."]; ok {
		rootID = old.ID
	} else {
		rootID, _ = utils.RandomString(16)
	}
	rootNode := &Node{
		ID:      rootID,
		Path:    ".",
		Name:    rootInfo.Name(),
		IsDir:   true,
		Size:    uint64(rootInfo.Size()),
		ModTime: rootInfo.ModTime(),
	}

	// 用于存储路径到节点ID的映射
	pathToID := make(map[string]string)
	pathToID["."] = rootNode.ID

	// 磁盘上实际存在的路径集合，遍历结束后据此清理失效节点
	seen := make(map[string]struct{})
	seen["."] = struct{}{}
	reusedHashes := 0

	// 使用并发安全的集合
	var allNodes []*Node
	var computedHashes int
	var mu sync.Mutex

	// 使用工作池处理节点收集
	var workerCount = runtime.NumCPU()
	nodeChan := make(chan *Node, 1000)
	var wg sync.WaitGroup

	// 启动工作池：并发计算文件哈希后再收集。
	// 哈希是 diff 比对的依据；校准模式下未变化的文件已带哈希，跳过重算
	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for node := range nodeChan {
				if !node.IsDir && node.Hash == "" {
					if hash, err := utils.CalcBlake3(filepath.Join(path, node.Path)); err != nil {
						// 哈希缺失的节点仍进树：客户端会确定性跳过它（不发注定失败的
						// 请求），但因节点存在，镜像侧已有副本不会被 --allow-delete 误删。
						// 同时登记进不可读列表，由 watcher 的恢复循环定期探测
						MarkUnreadable(filepath.Join(path, node.Path))
						log.Errorf("cannot read %s (%v); the file is excluded from sync and recovers automatically once readable", node.Path, err)
					} else {
						node.Hash = fmt.Sprintf("%x", hash)
						mu.Lock()
						computedHashes++
						mu.Unlock()
					}
				}
				mu.Lock()
				allNodes = append(allNodes, node)
				mu.Unlock()
			}
		}()
	}

	// 先添加根节点
	nodeChan <- rootNode

	// 使用WalkDir遍历
	walkErr := filepath.WalkDir(path, func(fullPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			log.Warnf("Error accessing path %s: %v", fullPath, walkErr)
			return nil
		}

		if fullPath == path {
			return nil
		}

		// 检查忽略列表
		relPath := utils.RelPath(config.StartPath, fullPath)
		if utils.IsIgnored(relPath, config.IgnoreFileList) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// 跳过符号链接：绝不追踪、绝不解引用。
		// 否则服务端会把链接目标（可能在同步根目录之外）的内容当作普通文件
		// 发给客户端，造成信息泄露/路径穿越；且符号链接的删除也无法被可靠检测。
		// WalkDir 用 Lstat 语义，d.Type() 能识别链接本身而不追踪目标
		if d.Type()&fs.ModeSymlink != 0 {
			log.Warnf("skipping symlink (not synced): %s", relPath)
			return nil
		}

		// 获取文件信息
		info, err := d.Info()
		if err != nil {
			log.Warnf("Error getting file info for %s: %v", fullPath, err)
			return nil
		}

		seen[relPath] = struct{}{}

		// 已缓存的节点复用 ID；size+mtime 未变的文件同时复用哈希
		id := ""
		hash := ""
		if old, ok := existing[relPath]; ok {
			id = old.ID
			if !info.IsDir() && old.Hash != "" &&
				old.Size == uint64(info.Size()) && old.ModTime.Equal(info.ModTime()) {
				hash = old.Hash
				reusedHashes++
			}
		} else {
			id, _ = utils.RandomString(16)
		}

		// 计算父节点路径
		parentPath := utils.RelPath(config.StartPath, filepath.Dir(fullPath))

		node := &Node{
			ID:       id,
			Path:     relPath,
			Name:     info.Name(),
			ParentID: pathToID[parentPath],
			IsDir:    info.IsDir(),
			Size:     uint64(info.Size()),
			ModTime:  info.ModTime(),
			Hash:     hash,
			Depth:    strings.Count(relPath, string(filepath.Separator)),
		}

		// 记录路径到ID的映射
		pathToID[relPath] = id

		// 发送到工作池
		nodeChan <- node
		return nil
	})

	close(nodeChan)
	wg.Wait()

	if walkErr != nil {
		log.Error("Error walking directory:", walkErr)
		return walkErr
	}

	// 批量写入数据库
	log.Infof("Collected %d nodes with concurrent processing, writing to database...", len(allNodes))

	// 分批写入数据库
	batchSize := 1000
	for i := 0; i < len(allNodes); i += batchSize {
		end := i + batchSize
		end = min(end, len(allNodes))
		batch := allNodes[i:end]
		if err := AddNodes(batch); err != nil {
			log.Error("Failed to add nodes to database:", err)
			return err
		}
	}

	// 清理缓存中磁盘上已不存在的节点（进程离线期间被删除的文件/目录）
	var stale []string
	for p := range existing {
		if _, ok := seen[p]; !ok {
			stale = append(stale, p)
		}
	}
	if len(stale) > 0 {
		if err := DeleteNodes(stale); err != nil {
			return fmt.Errorf("failed to prune stale nodes: %w", err)
		}
	}

	log.Infof("file tree build completed, time taken: %d ms (hashes reused %d, computed %d, stale pruned %d)",
		time.Now().UnixMilli()-startTime, reusedHashes, computedHashes, len(stale))

	fileCount, _ := GetMeta("file_count")
	dirCount, _ := GetMeta("dir_count")
	log.Infof("file tree build completed - dirs: %d, files: %d", dirCount, fileCount)

	return nil
}
