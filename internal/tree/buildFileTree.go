package tree

import (
	"io/fs"
	"local-mirror/config"
	"local-mirror/pkg/utils"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

var (
	RecentChangedDirs []string
	maxRecentDirs     = 100
	mu                sync.Mutex
)

func RemoveRecentChangedDir(dirPath string) {
	mu.Lock()
	defer mu.Unlock()

	filtered := RecentChangedDirs[:0]
	for _, v := range RecentChangedDirs {
		if !strings.HasPrefix(v, dirPath) {
			filtered = append(filtered, v)
		}
	}
	RecentChangedDirs = filtered
}
func AddRecentChangedDir(dirPath string) {
	mu.Lock()
	defer mu.Unlock()

	// 查找是否已存在
	has := -1
	for i, v := range RecentChangedDirs {
		if v == dirPath {
			has = i
			break
		}
	}
	if has > -1 {
		copy(RecentChangedDirs[1:has+1], RecentChangedDirs[:has])
		RecentChangedDirs[0] = dirPath
	} else {
		RecentChangedDirs = append([]string{dirPath}, RecentChangedDirs...)
		if len(RecentChangedDirs) > maxRecentDirs {
			RecentChangedDirs = RecentChangedDirs[:maxRecentDirs]
		}
	}
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

	// 启动工作池
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for node := range nodeChan {
				mu.Lock()
				allNodes = append(allNodes, node)
				mu.Unlock()
			}
		}(i)
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
		relPath := strings.Replace(fullPath, config.StartPath, ".", 1)
		for _, ignorePattern := range config.IgnoreFileList {
			if strings.Contains(relPath, ignorePattern) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
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
		parentPath := strings.Replace(filepath.Dir(fullPath), config.StartPath, ".", 1)

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
		if end > len(allNodes) {
			end = len(allNodes)
		}
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
