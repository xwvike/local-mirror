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
		}
	})
	addChangeTimerActive = true
}

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

	// 创建根节点
	uuid, _ := utils.RandomString(16)
	rootNode := &Node{
		ID:       uuid,
		Path:     ".",
		Name:     rootInfo.Name(),
		ParentID: "",
		IsDir:    true,
		Size:     uint64(rootInfo.Size()),
		ModTime:  rootInfo.ModTime(),
		Hash:     "",
		Depth:    0, // 根节点深度为0
	}

	// 用于存储路径到节点ID的映射
	pathToID := make(map[string]string)
	pathToID["."] = rootNode.ID

	// 使用并发安全的集合
	var allNodes []*Node
	var mu sync.Mutex

	// 使用工作池处理节点收集
	var workerCount = runtime.NumCPU()
	nodeChan := make(chan *Node, 1000)
	var wg sync.WaitGroup

	// 启动工作池：并发计算文件哈希后再收集。
	// 哈希是 diff 比对的依据，没有它客户端无法判断文件内容是否变化
	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for node := range nodeChan {
				if !node.IsDir {
					if hash, err := utils.CalcBlake3(filepath.Join(path, node.Path)); err != nil {
						log.Warnf("Failed to hash file %s: %v", node.Path, err)
					} else {
						node.Hash = fmt.Sprintf("%x", hash)
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

		// 获取文件信息
		info, err := d.Info()
		if err != nil {
			log.Warnf("Error getting file info for %s: %v", fullPath, err)
			return nil
		}

		// 创建节点
		uuid, _ := utils.RandomString(16)

		// 计算父节点路径
		parentPath := utils.RelPath(config.StartPath, filepath.Dir(fullPath))

		// 获取父节点ID
		parentID := pathToID[parentPath]

		node := &Node{
			ID:       uuid,
			Path:     relPath,
			Name:     info.Name(),
			ParentID: parentID,
			IsDir:    info.IsDir(),
			Size:     uint64(info.Size()),
			ModTime:  info.ModTime(),
			Hash:     "",
			Depth:    strings.Count(relPath, string(filepath.Separator)),
		}

		// 记录路径到ID的映射
		pathToID[relPath] = uuid

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

	log.Infof("file tree build completed with concurrent WalkDir, time taken: %d ms", time.Now().UnixMilli()-startTime)

	fileCount, _ := GetMeta("file_count")
	dirCount, _ := GetMeta("dir_count")
	log.Infof("file tree build completed - dirs: %d, files: %d", dirCount, fileCount)

	return nil
}
