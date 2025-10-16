package app

import (
	"errors"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/internal/network"
	"local-mirror/internal/transport"
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

// Keep NextLevel as global to maintain compatibility with external callers
var NextLevel = stack.NewStack[DiffResult]()

var (
	taskMutex    sync.Mutex // 确保任务串行执行
	isTaskActive bool       // 标识当前是否有任务在执行
)

// handleConnectionError wraps connection error handling to reduce duplication
func handleConnectionError(err error, fileClient *network.FileClient) error {
	if errors.Is(err, appError.ErrConnection) {
		fileClient.ConnectionClose()
	}
	return err
}

// createNodeFromDiff creates a tree node from diff info to avoid code duplication
func createNodeFromDiff(v DiffResult, hash string) *tree.Node {
	uuid, _ := utils.RandomString(16)
	return &tree.Node{
		ID:       uuid,
		Path:     v.Path,
		Name:     v.Name,
		ParentID: v.ParentID,
		IsDir:    v.IsDir,
		Size:     v.Size,
		ModTime:  time.Now(),
		Hash:     hash,
	}
}

func executeTaskWithClient(taskName string, fileClient *network.FileClient, taskFunc func(*network.FileClient) error) error {
	if fileClient.State == transport.Deprecated {
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
	if err := os.MkdirAll(v.Path, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", v.Path, err)
	}

	hasPath, err := tree.HasPath(v.Path)
	if err != nil {
		log.Fatalf("Error checking path %s: %v", v.Path, err)
		return err
	}

	if !hasPath {
		node := createNodeFromDiff(v, "")
		tree.AddNodes([]*tree.Node{node})
	}

	NextLevel.Push(v)
	return nil
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
	tree.AddNodes([]*tree.Node{fileNode})
	log.Infof("File downloaded successfully: %s", v.Path)
	return nil
}

func getDirectory(fileClient *network.FileClient, path string) error {
	treejson, err := fileClient.GetRealityTree(path)
	if err != nil {
		return handleConnectionError(err, fileClient)
	}

	log.Debug("start analyzing diff 🫨")
	if err = Diff(treejson, path); err != nil {
		return fmt.Errorf("error analyzing diff for path %s: %w", path, err)
	}

	log.Infof("Diff count: %d", diffQueue.Size())
	for diffQueue.Size() > 0 {
		v, has := diffQueue.Pop()
		if !has {
			log.Warn("Diff queue is empty")
			continue
		}

		log.Debugf("Processing diff item: %v 【%d】remaining", v, diffQueue.Size())
		if err := processDiffItem(v, fileClient); err != nil {
			log.Errorf("Error processing diff item %v: %v", v, err)
			continue
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

	if fileClient.State == transport.Online {
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
	if err := executeTaskWithClient("初始化全量扫描", fileClient, func(client *network.FileClient) error {
		return fullScan(client)
	}); err != nil {
		return err
	}

	cooldownSeconds := time.Duration(*config.CoolDown) * time.Second
	fullScanInterval := cooldownSeconds
	changeTrackInterval := time.Duration(*config.DiffInterval) * time.Second

	fullScanChan := make(chan struct{})
	changeChan := make(chan struct{})

	go func() {
		fullScanTicker := time.NewTicker(fullScanInterval)
		defer fullScanTicker.Stop()

		for range fullScanTicker.C {
			taskMutex.Lock()
			if !isTaskActive {
				taskMutex.Unlock()
				fullScanChan <- struct{}{}
			} else {
				taskMutex.Unlock()
				log.Info("全量扫描跳过 - 有任务正在执行")
			}
		}
	}()

	go func() {
		changeTicker := time.NewTicker(changeTrackInterval)
		defer changeTicker.Stop()

		for range changeTicker.C {
			taskMutex.Lock()
			if !isTaskActive {
				taskMutex.Unlock()
				changeChan <- struct{}{}
			} else {
				taskMutex.Unlock()
				log.Info("变更追踪跳过 - 有任务正在执行")
			}

		}
	}()

	for {
		select {
		case <-fullScanChan:
			if err := executeTaskWithClient("全量扫描", fileClient, func(client *network.FileClient) error {
				return fullScan(client)
			}); err != nil {
				return err
			}

		case <-changeChan:
			if err := executeTaskWithClient("变更追踪", fileClient, func(client *network.FileClient) error {
				return TrackingChanges(client)
			}); err != nil {
				return err
			}
		}
	}
}

func fullScan(fileClient *network.FileClient) error {
	startTime := time.Now().UnixMilli()

	// Clear the stack and start with root node
	for NextLevel.Size() > 0 {
		NextLevel.Pop()
	}

	rootNode := DiffResult{
		Path:   ".",
		IsDir:  true,
		Action: "create",
		Name:   "root",
		Size:   0,
	}
	NextLevel.Push(rootNode)

	for NextLevel.Size() > 0 {
		v, _ := NextLevel.Pop()
		log.Infof("Processing next level item: %v 【%d】remaining", v, NextLevel.Size())

		if !v.IsDir {
			log.Error("Unexpected file type in NextLevel stack, expected directory but got file:", v.Path)
			continue
		}

		err := getDirectory(fileClient, v.Path)
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

	elapsedSeconds := (time.Now().UnixMilli() - startTime) / 1000
	log.Info("Full scan completed, total time taken:", elapsedSeconds, "seconds")
	return nil
}

func TrackingChanges(fileClient *network.FileClient) error {
	change, err := fileClient.GetTreeChange()
	if err != nil {
		return handleConnectionError(err, fileClient)
	}

	if len(change) == 0 {
		log.Info("No changes detected in the tree")
		return nil
	}
	allPaths := extractMinimalPathsFromChanges(change)
	for _, v := range allPaths {
		log.Infof("Processing change: %v", v)
		err := getDirectory(fileClient, v)
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
