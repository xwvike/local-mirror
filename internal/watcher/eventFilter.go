package watcher

import (
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/tree"
	"local-mirror/pkg/utils"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

// 事件在 1 秒静默窗口内合并批量落库；缓存被监听 goroutine 和
// timer 回调 goroutine 同时读写，必须加锁
var (
	eventMu           sync.Mutex
	createEventCache  []*tree.Node
	createTimer       *time.Timer
	createTimerActive bool
	deleteEventCache  []string
	deleteTimer       *time.Timer
	deleteTimerActive bool
)

func eventFilter(event fsnotify.Event) {
	relPath := utils.RelPath(config.StartPath, event.Name)
	if utils.IsIgnored(relPath, config.IgnoreFileList) {
		return
	}
	nodeDir := filepath.Dir(relPath)
	fatherNode, err := tree.GetNodeByPath(nodeDir)
	if err != nil {
		log.Errorf("Incomplete directory tree, unable to find parent node for %s: %v", nodeDir, err)
		return
	}

	switch {
	case event.Has(fsnotify.Create) || event.Has(fsnotify.Write):
		fileInfo, err := os.Stat(event.Name)
		if err != nil {
			log.Error("Error getting file info:", err)
			return
		}
		hash := ""
		if !fileInfo.IsDir() {
			if h, hashErr := utils.CalcBlake3(event.Name); hashErr != nil {
				log.Warnf("Failed to hash file %s: %v", event.Name, hashErr)
			} else {
				hash = fmt.Sprintf("%x", h)
			}
		}
		uuid, _ := utils.RandomString(16)
		newLeaf := &tree.Node{
			ID:       uuid,
			Path:     relPath,
			Name:     filepath.Base(event.Name),
			ParentID: fatherNode.ID,
			IsDir:    fileInfo.IsDir(),
			Size:     uint64(fileInfo.Size()),
			ModTime:  fileInfo.ModTime(),
			Hash:     hash,
			Depth:    strings.Count(relPath, string(filepath.Separator)),
		}
		if fileInfo.IsDir() {
			if event.Has(fsnotify.Create) {
				GlobalScoreWatch.addHeat(newLeaf.Path, newLeaf)
			}
			// 新目录的内容可能在 watch 建立之前就已写入（如 mkdir -p 或整体移入），
			// 这些内容永远不会再有事件；立即落库目录节点并递归扫描其内容
			eventMu.Lock()
			createEventCache = append(createEventCache, newLeaf)
			eventMu.Unlock()
			flushCreateEvents()
			tree.AddRecentChangedDir(fatherNode.Path)
			scanNewDirContents(event.Name)
			return
		}

		eventMu.Lock()
		createEventCache = append(createEventCache, newLeaf)
		if createTimerActive {
			createTimer.Stop()
		}
		createTimer = time.AfterFunc(1*time.Second, flushCreateEvents)
		createTimerActive = true
		eventMu.Unlock()

		tree.AddRecentChangedDir(fatherNode.Path)
	case event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename):
		// Rename 事件表示旧路径消失，等同删除；若产生了新路径会另收到 Create 事件
		GlobalScoreWatch.removeHeat(relPath)

		eventMu.Lock()
		deleteEventCache = append(deleteEventCache, relPath)
		if deleteTimerActive {
			deleteTimer.Stop()
		}
		deleteTimer = time.AfterFunc(1*time.Second, flushDeleteEvents)
		deleteTimerActive = true
		eventMu.Unlock()

		tree.AddRecentChangedDir(fatherNode.Path)
	}
}

// scanNewDirContents 对新出现的目录做一次浅层扫描，
// 为每个条目合成 Create 事件；子目录在 eventFilter 中递归处理
func scanNewDirContents(fullPath string) {
	entries, err := os.ReadDir(fullPath)
	if err != nil {
		log.Warnf("Failed to scan new directory %s: %v", fullPath, err)
		return
	}
	for _, entry := range entries {
		eventFilter(fsnotify.Event{
			Name: filepath.Join(fullPath, entry.Name()),
			Op:   fsnotify.Create,
		})
	}
}

func flushCreateEvents() {
	eventMu.Lock()
	batch := createEventCache
	createEventCache = nil
	createTimerActive = false
	eventMu.Unlock()

	if len(batch) == 0 {
		return
	}
	if err := tree.AddNodes(batch); err != nil {
		log.Errorf("Failed to add nodes: %v", err)
	} else {
		log.Debugf("Added nodes count %d", len(batch))
	}
}

func flushDeleteEvents() {
	eventMu.Lock()
	batch := deleteEventCache
	deleteEventCache = nil
	deleteTimerActive = false
	eventMu.Unlock()

	if len(batch) == 0 {
		return
	}
	if err := tree.DeleteNodes(batch); err != nil {
		log.Errorf("Failed to delete nodes: %v", err)
	} else {
		log.Debugf("Deleted nodes count %d", len(batch))
	}
}
