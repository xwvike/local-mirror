package app

import (
	"local-mirror/app/model"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

type DynamicWatcher struct {
	leaf          *model.Leaf
	watcher       *fsnotify.Watcher
	allDirs       []string
	fixedWatchers []string
	hotWatchers   []string
	maxWatchers   uint16
	waitScan      time.Duration
	mu            sync.RWMutex
}

func NewDynamicWatcher(watcher *fsnotify.Watcher, leaf *model.Leaf, waitScan time.Duration) (*DynamicWatcher, error) {
	return &DynamicWatcher{
		leaf:          leaf,
		allDirs:       make([]string, 0),
		watcher:       watcher,
		fixedWatchers: make([]string, 0),
		hotWatchers:   make([]string, 0),
		maxWatchers:   uint16(256),
		waitScan:      waitScan,
	}, nil
}
func (dw *DynamicWatcher) Start() {
	dw.mu.Lock()
	defer dw.mu.Unlock()

	halfuLimit := uint16(dw.maxWatchers / 2)
	var watcherLevel uint16 = 0
	var fixedWatchersCount uint16 = 0

	for {
		watcherLevel++
		lastFixedWatcherCount := fixedWatchersCount
		fixedWatchersCount = uint16(len(dw.leaf.GetAllDirs(watcherLevel)))

		if fixedWatchersCount > halfuLimit || fixedWatchersCount <= lastFixedWatcherCount {
			if fixedWatchersCount > halfuLimit && watcherLevel > 1 {
				watcherLevel--
				fixedWatchersCount = uint16(len(dw.leaf.GetAllDirs(watcherLevel)))
			}
			break
		}
	}
	dw.fixedWatchers = dw.leaf.GetAllDirs(watcherLevel)
	log.Infof("DynamicWatcher: max open files limit is %d", dw.maxWatchers)
	log.Infof("DynamicWatcher: max watchers level is %d", watcherLevel)
	log.Infof("DynamicWatcher: fixed watchers count is %d", fixedWatchersCount)
	for _, dir := range dw.fixedWatchers {
		for _, v := range model.IgnoreFileList {
			if strings.Contains(dir, v) {
				continue
			}
		}
		err := dw.watcher.Add(dir)
		if err != nil {
			log.Fatalf("Failed to add watcher for %s: %v", dir, err)
		}
	}

}

func WatchFile(watcher *fsnotify.Watcher) {
	log.Info("setp 2 >> start watcher")
	dynamicWatcher, err := NewDynamicWatcher(watcher, model.RootLeaf, 1*time.Second)
	if err != nil {
		log.Fatalf("Failed to create dynamic watcher: %v", err)
	}
	dynamicWatcher.Start()
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				eventFilter(event, watcher)
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("error:", err)
			}
		}
	}()
}
