package app

import (
	"local-mirror/app/tree"
	"local-mirror/common/utils"
	"local-mirror/config"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

type DynamicWatcher struct {
	watcher     *fsnotify.Watcher
	watchers    []string
	maxWatchers uint16
	usedWatches uint16
	waitScan    time.Duration
	mu          sync.RWMutex
}

func NewDynamicWatcher(watcher *fsnotify.Watcher, waitScan time.Duration) (*DynamicWatcher, error) {
	var _maxWatches uint16 = 10240
	os := utils.BaseOSInfo().OS
	switch os {
	case "darwin":
		maxfilesperprocCmd := exec.Command("sysctl", "-n", "kern.maxfilesperproc")
		maxfilesCmd := exec.Command("sysctl", "-n", "kern.maxfiles")
		maxfilesperproc, maxfilesperprocErr := maxfilesperprocCmd.Output()
		maxfiles, maxfilesErr := maxfilesCmd.Output()
		if maxfilesperprocErr != nil || maxfilesErr != nil {
			_maxWatches = 1024
		} else {
			maxFilesPerProcInt, err1 := strconv.ParseUint(strings.TrimSpace(string(maxfilesperproc)), 10, 16)
			maxFilesInt, err2 := strconv.ParseUint(strings.TrimSpace(string(maxfiles)), 10, 16)
			if err1 != nil || err2 != nil {
				_maxWatches = 1024
			} else {
				_maxWatches = min(uint16(maxFilesPerProcInt), uint16(maxFilesInt))
			}
		}
	case "linux":
		maxWatchesCmd := exec.Command("sh", "-c", "cat /proc/sys/fs/inotify/max_user_watches")
		maxWatchesOutput, err := maxWatchesCmd.Output()
		if err != nil {
			log.Error("Failed to get max user watches:", err)
			_maxWatches = 1024
		} else {
			maxWatchesInt, err := strconv.ParseUint(strings.TrimSpace(string(maxWatchesOutput)), 10, 16)
			if err != nil {
				log.Error("Failed to parse max user watches:", err)
				_maxWatches = 1024
			} else {
				_maxWatches = uint16(maxWatchesInt)
			}
		}
	case "windows":
		// Windows does not have a direct equivalent, using a default value
		_maxWatches = 10240
	default:
		log.Warnf("Unsupported OS %s, using default max watchers value", os)
		_maxWatches = 1024
	}
	return &DynamicWatcher{
		watcher:     watcher,
		watchers:    make([]string, 0),
		maxWatchers: _maxWatches / 2,
		usedWatches: 0,
		waitScan:    waitScan,
		mu:          sync.RWMutex{},
	}, nil
}
func (dw *DynamicWatcher) Start() {
	dw.mu.Lock()
	defer dw.mu.Unlock()
	rootNodes, err := tree.GetDirContents(".")
	if err != nil {
		log.Error("Failed to get directory contents:", err)
		return
	}
	osType := utils.BaseOSInfo().OS
	switch osType {
	case "darwin":
		if len(rootNodes)+1+int(dw.usedWatches) > int(dw.maxWatchers) {
			log.Warnf("Too many files to watch, max is %d, current is %d", dw.maxWatchers, len(rootNodes)+1+int(dw.usedWatches))
		} else {
			dw.watcher.Add(config.StartPath)
			dw.usedWatches = uint16(len(rootNodes)+1) + dw.usedWatches
		}
	case "linux":
		dw.watcher.Add(config.StartPath)
		dw.usedWatches++
	case "windows":
		dw.watcher.Add(config.StartPath)
		dw.usedWatches++
	default:
		log.Warnf("Unsupported OS %s, cannot add root directory to watcher", osType)
	}
}

func WatchFile(watcher *fsnotify.Watcher) {
	log.Info("Starting file watcher...")
	dynamicWatcher, err := NewDynamicWatcher(watcher, 1*time.Second)
	if err != nil {
		log.Error("Failed to create dynamic watcher:", err)
	}
	dynamicWatcher.Start()
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				eventFilter(event)
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Errorf("watcher error: %v", err)
			}
		}
	}()
}
