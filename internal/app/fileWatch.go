package app

import (
	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"runtime"
	"strings"
	"sync"
	"time"
)

type DynamicWatcher struct {
	leaf          *Leaf
	watcher       *fsnotify.Watcher
	allDirs       []string
	fixedWatchers []string
	hotWatchers   []string
	maxWatchers   uint16
	waitScan      time.Duration
	mu            sync.RWMutex
}

func getMaxOpenFiles() uint64 {
	if runtime.GOOS == "windows" {
		return 65535 // Windows has a high limit by default
	}
	var rLimit unix.Rlimit
	err := unix.Getrlimit(unix.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		log.Fatalf("Failed to get rlimit: %v", err)
		return 256
	}
	return rLimit.Cur
}

func NewDynamicWatcher(watcher *fsnotify.Watcher, leaf *Leaf, waitScan time.Duration) (*DynamicWatcher, error) {
	return &DynamicWatcher{
		leaf:          leaf,
		allDirs:       make([]string, 0),
		watcher:       watcher,
		fixedWatchers: make([]string, 0),
		hotWatchers:   make([]string, 0),
		maxWatchers:   uint16(getMaxOpenFiles()),
		waitScan:      waitScan,
	}, nil
}
func (dw *DynamicWatcher) Start() {
	dw.mu.Lock()
	defer dw.mu.Unlock()

	ulimit := uint16(getMaxOpenFiles())
	halfuLimit := uint16(ulimit / 5)
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
	log.Infof("DynamicWatcher: max open files limit is %d", ulimit)
	log.Infof("DynamicWatcher: max watchers level is %d", watcherLevel)
	log.Infof("DynamicWatcher: fixed watchers count is %d", fixedWatchersCount)
	for _, dir := range dw.fixedWatchers {
		for _, v := range ignoreFileList {
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
	dynamicWatcher, err := NewDynamicWatcher(watcher, rootLeaf, 1*time.Second)
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
