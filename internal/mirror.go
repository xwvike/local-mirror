package app

import (
	"errors"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/internal/network"
	"local-mirror/internal/tree"
	"local-mirror/pkg/stack"
	"local-mirror/pkg/utils"
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

var (
	taskMutex    sync.Mutex // 确保任务串行执行
	isTaskActive bool       // 标识当前是否有任务在执行
)

// heartbeatInterval 空闲心跳间隔。必须明显小于服务端的空闲超时
// （network.ClientIdleTimeout），否则连接会在两次心跳之间被服务端断开
const heartbeatInterval = 30 * time.Second

// lastChangeCursor 记录变更查询已覆盖到的时间点（unix 秒）。
// 前后两次查询以此衔接，即使某次追踪被跳过也不会漏掉中间的变更。
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

	isTaskActive = true
	defer func() { isTaskActive = false }()

	log.Infof("开始执行任务: %s", taskName)
	startTime := time.Now()

	if err := taskFunc(fileClient); err != nil {
		log.Errorf("任务执行失败 %s: %v", taskName, err)
		if errors.Is(err, appError.ErrConnection) {
			return fmt.Errorf("client became deprecated during task: %w", err)
		}
	}

	duration := time.Since(startTime)
	log.Infof("任务完成: %s, 耗时: %v", taskName, duration)
	return nil
}

// processDiffItem handles a single diff item (file or directory)
func processDiffItem(v DiffResult, fileClient *network.FileClient) error {
	switch v.Action {
	case "delete":
		err := os.RemoveAll(filepath.Join(config.StartPath, v.Path))
		if err == nil {
			tree.DeleteNode(v.Path)
		}
		return err

	case "create", "modify":
		if v.IsDir {
			return processDirectoryDiff(v)
		}
		return processFileDiff(v, fileClient)

	default:
		log.Warnf("Unknown action type: %s", v.Action)
		return nil
	}
}

func processDirectoryDiff(v DiffResult) error {
	// v.Path 是相对路径，必须拼接 StartPath 才能在正确的位置创建目录
	fullPath := filepath.Join(config.StartPath, v.Path)
	if err := os.MkdirAll(fullPath, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", fullPath, err)
	}

	// AddNodes 对已存在路径按更新处理，无需先查询
	node := createNodeFromDiff(v, "")
	return tree.AddNodes([]*tree.Node{node})
}

func processFileDiff(v DiffResult, fileClient *network.FileClient) error {
	hash, err := fileClient.DownloadFile(v.Path)
	if err != nil {
		if errors.Is(err, appError.ErrConnection) {
			fileClient.ConnectionClose()
			return err
		}
		log.Errorf("Error downloading file %s: %v", v.Path, err)
		return err
	}

	fileNode := createNodeFromDiff(v, hash)
	if err := tree.AddNodes([]*tree.Node{fileNode}); err != nil {
		return err
	}
	log.Infof("File downloaded successfully: %s", v.Path)
	return nil
}

// getDirectory 同步单个目录：拉取服务端目录列表、执行差异处理，
// 并把需要继续下钻的子目录压入 NextLevel。
// recurseAll 为 true 时所有子目录都下钻（全量扫描的安全网语义）；
// 为 false 时只下钻本次新建/变更的子目录。
func getDirectory(fileClient *network.FileClient, path string, recurseAll bool) error {
	treejson, err := fileClient.GetRealityTree(path)
	if err != nil {
		return handleConnectionError(err, fileClient)
	}

	realityNodes, err := UnmarshalRealityTree(treejson)
	if err != nil {
		return fmt.Errorf("error parsing tree for path %s: %w", path, err)
	}

	diffs, err := Diff(realityNodes, path)
	if err != nil {
		return fmt.Errorf("error analyzing diff for path %s: %w", path, err)
	}

	log.Infof("Diff count for %s: %d", path, len(diffs))
	diffDirs := make(map[string]bool)
	for _, v := range diffs {
		if err := processDiffItem(v, fileClient); err != nil {
			// 连接断了，本目录剩余项留给重试；其他错误跳过单项继续
			if errors.Is(err, appError.ErrConnection) {
				return err
			}
			log.Errorf("Error processing diff item %v: %v", v, err)
			continue
		}
		if v.IsDir && v.Action != "delete" {
			diffDirs[v.Path] = true
			NextLevel.Push(v)
		}
	}

	if recurseAll {
		for _, node := range realityNodes {
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

// drainNextLevel 逐层消费 NextLevel 中的目录，连接错误时重连并重试当前目录
func drainNextLevel(fileClient *network.FileClient, recurseAll bool) error {
	for NextLevel.Size() > 0 {
		v, _ := NextLevel.Pop()
		log.Debugf("Processing next level item: %v 【%d】remaining", v, NextLevel.Size())

		if !v.IsDir {
			log.Error("Unexpected file type in NextLevel stack, expected directory but got file:", v.Path)
			continue
		}

		err := getDirectory(fileClient, v.Path, recurseAll)
		if err == nil {
			continue
		}

		log.Errorf("Error processing directory %s: %v", v.Path, err)
		if errors.Is(err, appError.ErrConnection) {
			if reconnectErr := fileClient.Reconnect(); reconnectErr != nil {
				return err
			}
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
		if err := runMirrorTasks(fileClient); err != nil {
			log.Errorf("Error running mirror tasks: %v", err)
			fileClient.ConnectionClose()
			time.Sleep(5 * time.Second)
			continue
		}
	}
}

func runMirrorTasks(fileClient *network.FileClient) error {
	if err := executeTaskWithClient("初始化全量扫描", fileClient, fullScan); err != nil {
		return err
	}

	fullScanInterval := time.Duration(*config.CoolDown) * time.Second
	changeTrackInterval := time.Duration(*config.DiffInterval) * time.Second

	fullScanChan := make(chan struct{})
	changeChan := make(chan struct{})
	heartbeatChan := make(chan struct{})
	// done 在本函数返回时关闭，ticker goroutine 随之退出，
	// 否则每次重连都会泄漏一批永远阻塞的 goroutine
	done := make(chan struct{})
	defer close(done)

	tickForward := func(interval time.Duration, ch chan struct{}, name string) {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				taskMutex.Lock()
				busy := isTaskActive
				taskMutex.Unlock()
				if busy {
					log.Infof("%s跳过 - 有任务正在执行", name)
					continue
				}
				select {
				case ch <- struct{}{}:
				case <-done:
					return
				}
			case <-done:
				return
			}
		}
	}

	go tickForward(fullScanInterval, fullScanChan, "全量扫描")
	go tickForward(changeTrackInterval, changeChan, "变更追踪")
	// 心跳与其他任务同走 executeTaskWithClient 串行：
	// 协议是同步请求-响应模型，同一连接上不允许并发收发
	go tickForward(heartbeatInterval, heartbeatChan, "心跳")

	for {
		select {
		case <-fullScanChan:
			if err := executeTaskWithClient("全量扫描", fileClient, fullScan); err != nil {
				return err
			}

		case <-changeChan:
			if err := executeTaskWithClient("变更追踪", fileClient, TrackingChanges); err != nil {
				return err
			}

		case <-heartbeatChan:
			if err := executeTaskWithClient("心跳", fileClient, func(c *network.FileClient) error {
				return c.Ping()
			}); err != nil {
				return err
			}
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

	// 全量扫描已覆盖到扫描开始时刻，变更游标从这里接力；
	// 用开始时间而非结束时间，扫描期间发生的变更下次仍会被查到
	lastChangeCursor = startTime.Unix()

	log.Infof("Full scan completed, total time taken: %v", time.Since(startTime))
	return nil
}

func TrackingChanges(fileClient *network.FileClient) error {
	endTime := time.Now().Unix()
	change, err := fileClient.GetTreeChange(lastChangeCursor, endTime)
	if err != nil {
		return handleConnectionError(err, fileClient)
	}

	if len(change) == 0 {
		log.Info("No changes detected in the tree")
		lastChangeCursor = endTime
		return nil
	}
	allPaths := extractMinimalPathsFromChanges(change)
	NextLevel.Clear()
	for _, v := range allPaths {
		log.Infof("Processing change: %v", v)
		err := getDirectory(fileClient, v, false)
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
	lastChangeCursor = endTime
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
