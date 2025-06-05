package main

import (
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// RotatingWatcher 轮询监听器
type RotatingWatcher struct {
	watcher        *fsnotify.Watcher
	allFolders     []string        // 所有需要监听的文件夹
	currentWatched map[string]bool // 当前正在监听的文件夹
	maxWatchers    int             // 最大同时监听数量
	rotateInterval time.Duration   // 轮换间隔
	eventChan      chan fsnotify.Event
	errorChan      chan error
	stopChan       chan struct{}
	mu             sync.RWMutex
	currentIndex   int // 当前轮询到的索引
}

// NewRotatingWatcher 创建新的轮询监听器
func NewRotatingWatcher(folders []string, maxWatchers int, rotateInterval time.Duration) (*RotatingWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	return &RotatingWatcher{
		watcher:        watcher,
		allFolders:     folders,
		currentWatched: make(map[string]bool),
		maxWatchers:    maxWatchers,
		rotateInterval: rotateInterval,
		eventChan:      make(chan fsnotify.Event, 1000),
		errorChan:      make(chan error, 100),
		stopChan:       make(chan struct{}),
		currentIndex:   0,
	}, nil
}

// Start 开始监听
func (rw *RotatingWatcher) Start() {
	// 初始添加第一批监听的文件夹
	rw.addInitialWatches()

	// 启动事件处理协程
	go rw.handleEvents()

	// 启动轮换协程
	go rw.rotateWatches()

	log.Printf("开始监听 %d 个文件夹，最大同时监听 %d 个，轮换间隔 %v",
		len(rw.allFolders), rw.maxWatchers, rw.rotateInterval)
}

// addInitialWatches 添加初始监听
func (rw *RotatingWatcher) addInitialWatches() {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	count := 0
	for i, folder := range rw.allFolders {
		if count >= rw.maxWatchers {
			break
		}

		if err := rw.watcher.Add(folder); err != nil {
			log.Printf("添加监听失败 %s: %v", folder, err)
			continue
		}

		rw.currentWatched[folder] = true
		count++
		rw.currentIndex = i + 1
	}

	log.Printf("初始添加了 %d 个文件夹监听", count)
}

// rotateWatches 轮换监听的文件夹
func (rw *RotatingWatcher) rotateWatches() {
	ticker := time.NewTicker(rw.rotateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rw.performRotation()
		case <-rw.stopChan:
			return
		}
	}
}

// performRotation 执行一次轮换
func (rw *RotatingWatcher) performRotation() {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if len(rw.allFolders) <= rw.maxWatchers {
		// 如果总文件夹数不超过最大监听数，不需要轮换
		return
	}

	// 计算需要轮换的数量（每次轮换一部分）
	rotateCount := rw.maxWatchers / 4 // 每次轮换25%
	if rotateCount < 1 {
		rotateCount = 1
	}

	// 移除一些旧的监听
	removedCount := 0
	for folder := range rw.currentWatched {
		if removedCount >= rotateCount {
			break
		}

		if err := rw.watcher.Remove(folder); err != nil {
			log.Printf("移除监听失败 %s: %v", folder, err)
		} else {
			delete(rw.currentWatched, folder)
			removedCount++
		}
	}

	// 添加新的监听
	addedCount := 0
	startIndex := rw.currentIndex

	for i := 0; i < len(rw.allFolders) && addedCount < rotateCount; i++ {
		folderIndex := (startIndex + i) % len(rw.allFolders)
		folder := rw.allFolders[folderIndex]

		// 如果已经在监听，跳过
		if rw.currentWatched[folder] {
			continue
		}

		if err := rw.watcher.Add(folder); err != nil {
			log.Printf("添加监听失败 %s: %v", folder, err)
			continue
		}

		rw.currentWatched[folder] = true
		addedCount++
		rw.currentIndex = (folderIndex + 1) % len(rw.allFolders)
	}

	if removedCount > 0 || addedCount > 0 {
		log.Printf("轮换完成: 移除 %d 个，添加 %d 个，当前监听 %d 个",
			removedCount, addedCount, len(rw.currentWatched))
	}
}

// handleEvents 处理文件系统事件
func (rw *RotatingWatcher) handleEvents() {
	for {
		select {
		case event, ok := <-rw.watcher.Events:
			if !ok {
				return
			}
			// 转发事件到外部通道
			select {
			case rw.eventChan <- event:
			default:
				log.Printf("事件通道已满，丢弃事件: %s", event.Name)
			}

		case err, ok := <-rw.watcher.Errors:
			if !ok {
				return
			}
			// 转发错误到外部通道
			select {
			case rw.errorChan <- err:
			default:
				log.Printf("错误通道已满，丢弃错误: %v", err)
			}

		case <-rw.stopChan:
			return
		}
	}
}

// Events 返回事件通道
func (rw *RotatingWatcher) Events() <-chan fsnotify.Event {
	return rw.eventChan
}

// Errors 返回错误通道
func (rw *RotatingWatcher) Errors() <-chan error {
	return rw.errorChan
}

// GetCurrentWatched 获取当前监听的文件夹列表
func (rw *RotatingWatcher) GetCurrentWatched() []string {
	rw.mu.RLock()
	defer rw.mu.RUnlock()

	var folders []string
	for folder := range rw.currentWatched {
		folders = append(folders, folder)
	}
	return folders
}

// Stop 停止监听
func (rw *RotatingWatcher) Stop() error {
	close(rw.stopChan)
	return rw.watcher.Close()
}

// 使用示例
func main() {
	// 生成测试文件夹列表
	var folders []string
	for i := 0; i < 100000; i++ {
		folders = append(folders, filepath.Join("/tmp/test", fmt.Sprintf("folder_%d", i)))
	}

	// 创建轮询监听器，最多同时监听512个文件夹，每30秒轮换一次
	rotatingWatcher, err := NewRotatingWatcher(folders, 512, 30*time.Second)
	if err != nil {
		log.Fatal(err)
	}

	// 开始监听
	rotatingWatcher.Start()

	// 处理事件
	go func() {
		for {
			select {
			case event := <-rotatingWatcher.Events():
				log.Printf("文件事件: %s %s", event.Op, event.Name)

			case err := <-rotatingWatcher.Errors():
				log.Printf("监听错误: %v", err)
			}
		}
	}()

	// 定期打印状态
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			watched := rotatingWatcher.GetCurrentWatched()
			log.Printf("当前监听 %d 个文件夹", len(watched))
		}
	}()

	// 运行程序
	select {}
}
