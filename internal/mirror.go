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

func executeTask(taskName string, taskFunc func() error) {
	taskMutex.Lock()
	defer taskMutex.Unlock()

	isTaskActive = true
	defer func() { isTaskActive = false }()

	log.Infof("开始执行任务: %s", taskName)
	startTime := time.Now()

	if err := taskFunc(); err != nil {
		log.Errorf("任务执行失败 %s: %v", taskName, err)
	}

	duration := time.Since(startTime)
	log.Infof("任务完成: %s, 耗时: %v", taskName, duration)
}

// processDiffItem handles a single diff item (file or directory)
func processDiffItem(v DiffResult, fileClient *network.FileClient) error {
	switch v.Action {
	case "delete":
		if err := os.RemoveAll(filepath.Join(config.StartPath, v.Path)); err != nil {
			tree.DeleteNode(v.Path)
		}
		return nil

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
		tree.
			([]*tree.Node{node})
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
			return err
		}
	}
	return nil
}

// ensureConnected makes sure we have a valid connection, retrying if needed
func ensureConnected() (*network.FileClient, error) {
	fileClient, err := InitConn()
	if err != nil {
		fileClient.ConnectionClose()
	}

	if fileClient.State != network.Offline {
		return fileClient, nil
	}

	// Connection is offline, try to reconnect
	log.Info("Connection offline, attempting to reconnect...")
	retryConnection := time.NewTicker(10 * time.Second)
	defer retryConnection.Stop()

	for range retryConnection.C {
		log.Info("Retrying connection to reality server...")
		fileClient, err = InitConn()
		if err == nil {
			log.Info("Successfully connected to reality server")
			return fileClient, nil
		}
		log.Errorf("Failed to connect to reality server: %v", err)
	}

	return fileClient, fmt.Errorf("failed to establish connection")
}

func Mirror() {
	log.Debug("step 3 >> start file client")
	fileClient, err := ensureConnected()
	if err != nil {
		log.Fatal("Failed to connect: ", err)
		return
	}

	// Initial full scan
	executeTask("初始化全量扫描", func() error {
		return fullScan(fileClient)
	})

	// Set up tickers for periodic operations
	cooldownSeconds := time.Duration(*config.CoolDown) * time.Second
	fullScanInterval := cooldownSeconds
	changeTrackInterval := 10 * time.Second

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
			executeTask("全量扫描", func() error {
				return fullScan(fileClient)
			})

		case <-changeChan:
			executeTask("变更追踪", func() error {
				return TrackingChanges(fileClient)
			})
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
				return fmt.Errorf("reconnection failed: %w", reconnectErr)
			}
			fileClient.State = network.Online
			NextLevel.Push(v) // Re-push the directory to retry
		}
	}

	elapsedSeconds := (time.Now().UnixMilli() - startTime) / 1000
	log.Info("Full scan completed, total time taken:", elapsedSeconds, "seconds")
	return nil
}

func TrackingChanges(fileClient *network.FileClient) error {
	change, err := fileClient.GetTreeChange(100)
	if err != nil {
		return handleConnectionError(err, fileClient)
	}

	if len(change) == 0 {
		log.Info("No changes detected in the tree")
		return nil
	}

	for _, v := range change {
		log.Infof("Processing change: %v", v)
		err := getDirectory(fileClient, v)
		if err == nil {
			continue
		}
		log.Errorf("Error processing directory %s: %v", v, err)
		if errors.Is(err, appError.ErrConnection) {
			if reconnectErr := fileClient.Reconnect(); reconnectErr != nil {
				return fmt.Errorf("reconnection failed: %w", reconnectErr)
			}
			fileClient.State = network.Online
		}
	}
	return nil
}
