package watcher

import (
	"local-mirror/config"
	"local-mirror/pkg/utils"
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
	osInfo := utils.GetOSInfo()
	switch osInfo.OS {
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
			_maxWatches = 8192
		} else {
			maxWatchesInt, err := strconv.ParseUint(strings.TrimSpace(string(maxWatchesOutput)), 10, 16)
			if err != nil {
				log.Error("Error parsing max user watches:", err)
				_maxWatches = 8192
			} else {
				_maxWatches = uint16(maxWatchesInt)
			}
		}
	default:
		_maxWatches = 1024
	}

	log.Infof("Maximum file watchers: %d", _maxWatches)

	return &DynamicWatcher{
		watcher:     watcher,
		watchers:    make([]string, 0),
		maxWatchers: _maxWatches,
		usedWatches: 0,
		waitScan:    waitScan,
	}, nil
}

func Initialize(watcher *fsnotify.Watcher) {
	dynamicWatcher, err := NewDynamicWatcher(watcher, 5*time.Second)
	if err != nil {
		log.Error("Failed to create dynamic watcher:", err)
		return
	}

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
				log.Error("Watcher error:", err)
			}
		}
	}()

	dynamicWatcher.addWatchesToDirectories()
}

func (dw *DynamicWatcher) addWatchesToDirectories() {
	ticker := time.NewTicker(dw.waitScan)
	defer ticker.Stop()

	for range ticker.C {
		dw.scanAndAddWatches()
	}
}

func (dw *DynamicWatcher) scanAndAddWatches() {
	// 实现扫描和添加监视逻辑
	log.Debug("Scanning for new directories to watch...")
}

func eventFilter(event fsnotify.Event) {
	log.Debugf("File event: %s %s", event.Name, event.Op.String())

	// 检查是否在忽略列表中
	for _, ignorePattern := range config.IgnoreFileList {
		if strings.Contains(event.Name, ignorePattern) {
			log.Debugf("Ignoring file event for: %s", event.Name)
			return
		}
	}

	// 处理文件系统事件
	// 这里可以添加具体的事件处理逻辑
}
