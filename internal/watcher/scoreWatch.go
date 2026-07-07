package watcher

import (
	"context"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/tree"
	"local-mirror/pkg/utils"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

type ScoreWatch struct {
	Watcher         *fsnotify.Watcher
	maxfilesperproc int //https://pkg.go.dev/github.com/fsnotify/fsnotify@v1.8.0#readme-linux
	tier1Limit      int
	tier2Interval   time.Duration
	// mu 保护 heatMap/tier1/tier2：事件处理、定期扫描、tier2 轮询三个 goroutine 都会访问
	mu      sync.Mutex
	heatMap map[string]*HeatScore
	tier1   []*HeatScore
	tier2   []*HeatScore
	ctx     context.Context
	cancel  context.CancelFunc
}

type HeatScore struct {
	Path       string
	Deepth     int
	Score      float64
	EventCount int
}

var GlobalScoreWatch *ScoreWatch

func InitWatcher(watcher *fsnotify.Watcher) error {
	// 系统上限值（如 macOS kern.maxfilesperproc 常见为 245760）远超 uint16，
	// 用 int 解析后再设置一个保守上限，避免溢出导致解析失败退回极小值
	const maxWatchesCap = 65536
	_maxWatches := 10240
	osType := utils.BaseOSInfo().OS
	switch osType {
	case "darwin":
		maxfilesperprocCmd := exec.Command("sysctl", "-n", "kern.maxfilesperproc")
		maxfilesCmd := exec.Command("sysctl", "-n", "kern.maxfiles")
		maxfilesperproc, maxfilesperprocErr := maxfilesperprocCmd.Output()
		maxfiles, maxfilesErr := maxfilesCmd.Output()
		if maxfilesperprocErr != nil || maxfilesErr != nil {
			_maxWatches = 1024
		} else {
			maxFilesPerProcInt, err1 := strconv.Atoi(strings.TrimSpace(string(maxfilesperproc)))
			maxFilesInt, err2 := strconv.Atoi(strings.TrimSpace(string(maxfiles)))
			if err1 != nil || err2 != nil {
				_maxWatches = 1024
			} else {
				_maxWatches = min(maxFilesPerProcInt, maxFilesInt, maxWatchesCap)
			}
		}
	case "linux":
		maxWatchesCmd := exec.Command("sh", "-c", "cat /proc/sys/fs/inotify/max_user_watches")
		maxWatchesOutput, err := maxWatchesCmd.Output()
		if err != nil {
			log.Error("Failed to get max user watches:", err)
			_maxWatches = 1024
		} else {
			maxWatchesInt, err := strconv.Atoi(strings.TrimSpace(string(maxWatchesOutput)))
			if err != nil {
				log.Error("Failed to parse max user watches:", err)
				_maxWatches = 1024
			} else {
				_maxWatches = min(maxWatchesInt, maxWatchesCap)
			}
		}
	case "windows":
		// Windows does not have a direct equivalent, using a default value
		_maxWatches = 10240
	default:
		log.Warnf("Unsupported OS %s, using default max watchers value", osType)
		_maxWatches = 1024
	}
	ctx, cancel := context.WithCancel(context.Background())
	GlobalScoreWatch = &ScoreWatch{
		Watcher:         watcher,
		maxfilesperproc: _maxWatches,
		tier1Limit:      _maxWatches / 2,
		tier2Interval:   3 * time.Second,
		heatMap:         make(map[string]*HeatScore),
		tier1:           make([]*HeatScore, 0),
		tier2:           make([]*HeatScore, 0),
		ctx:             ctx,
		cancel:          cancel,
	}

	err := GlobalScoreWatch.collectAll()
	if err != nil {
		return fmt.Errorf("ScoreWatch: %w", err)
	}

	go GlobalScoreWatch.handleEvents()

	go GlobalScoreWatch.intelligentScan()

	return nil
}

func (sw *ScoreWatch) collectAll() error {
	allDir, err := tree.GetAllDirNodes()
	if err != nil {
		return fmt.Errorf("failed to get all directory nodes: %w", err)
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()
	for _, dir := range allDir {
		path := dir.Path
		score := sw.calculateInitScore(path, dir)
		heatScore := &HeatScore{
			Path:       path,
			Deepth:     dir.Depth,
			Score:      score,
			EventCount: 0,
		}
		sw.heatMap[path] = heatScore
	}
	return nil
}

func (sw *ScoreWatch) calculateInitScore(path string, node *tree.Node) float64 {
	baseScore := 50.0

	pathWeight := sw.getPathHeuristics(path)
	timeWeight := sw.getTimeBasedScore(node.ModTime)
	depthWeight := math.Max(0.7, 1.0-float64(node.Depth)*0.1)
	// path 是 "./x" 形式的相对路径，必须拼接根目录，否则依赖进程 CWD
	fileCount, err := os.ReadDir(filepath.Join(config.StartPath, path))
	var fileCountInt int
	if err != nil {
		fileCountInt = 0
	} else {
		fileCountInt = len(fileCount)
	}
	fileWeight := 0.8 + math.Min(0.4, math.Log10(float64(fileCountInt+1))*0.2)
	totalScore := baseScore * (0.4*pathWeight + 0.3*timeWeight + 0.2*depthWeight + 0.1*fileWeight)

	return math.Max(10.0, math.Min(200.0, totalScore))
}

func (sw *ScoreWatch) getPathHeuristics(path string) float64 {
	pathLower := strings.ToLower(path)

	// 高价值目录模式
	highValuePatterns := []string{
		"document", "doc", "desktop", "download", "project",
		"code", "src", "source", "work", "workspace", "dev",
		"sync", "cloud", "dropbox", "onedrive", "googledrive",
	}

	// 低价值目录模式
	lowValuePatterns := []string{
		"cache", "temp", "tmp", "log", "logs", ".git",
		"node_modules", "vendor", "build", "dist", "bin",
		"__pycache__", ".vscode", ".idea",
	}

	for _, pattern := range highValuePatterns {
		if strings.Contains(pathLower, pattern) {
			return 2.0
		}
	}

	// 检查低价值模式
	for _, pattern := range lowValuePatterns {
		if strings.Contains(pathLower, pattern) {
			return 0.5
		}
	}

	return 1.0
}

func (sw *ScoreWatch) getTimeBasedScore(modTime time.Time) float64 {
	now := time.Now()
	hours := now.Sub(modTime).Hours()

	if hours < 1 {
		return 1.5
	} else if hours < 24 {
		return 1.3
	} else if hours < 168 {
		return 1.1
	} else {
		return 1.0
	}
}

func (sw *ScoreWatch) intelligentScan() {
	sw.performScan()
	go sw.monitorTier2()
	rollTicker := time.NewTicker(10 * time.Minute)
	defer rollTicker.Stop()

	for range rollTicker.C {
		sw.performScan()
	}
}

func (sw *ScoreWatch) performScan() {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	dirs := make([]*HeatScore, 0, len(sw.heatMap))
	for _, heat := range sw.heatMap {
		dirs = append(dirs, heat)
	}

	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].Score > dirs[j].Score
	})

	// 每轮扫描重建两个层级，防止 tier1/tier2 跨轮次累积重复条目
	oldTier1 := sw.tier1
	newTier1 := make([]*HeatScore, 0, len(dirs))
	var newTier2 []*HeatScore
	usedWatches := 0
	for i, heat := range dirs {
		if usedWatches >= sw.tier1Limit {
			// 剩余的低分目录全部进入 tier2 轮询
			newTier2 = append(newTier2, dirs[i:]...)
			break
		}
		fullPath := filepath.Join(config.StartPath, heat.Path)
		entries, err := os.ReadDir(fullPath)
		if err != nil {
			log.Warnf("Failed to read directory %s: %v", heat.Path, err)
			continue
		}
		switch utils.BaseOSInfo().OS {
		case "darwin":
			// kqueue 为目录及其每个条目各占一个描述符
			usedWatches += len(entries) + 1
		default:
			usedWatches++
		}
		if err := sw.Watcher.Add(fullPath); err != nil {
			log.Warnf("Failed to watch directory %s: %v", fullPath, err)
			continue
		}
		newTier1 = append(newTier1, heat)
	}
	sw.tier1 = newTier1
	sw.tier2 = newTier2
	sw.removeOldWatchers(oldTier1, newTier1)
}

func (sw *ScoreWatch) removeOldWatchers(oldTier1, newTier1 []*HeatScore) {
	newTier1Map := make(map[string]struct{})
	for _, heat := range newTier1 {
		newTier1Map[heat.Path] = struct{}{}
	}

	for _, heat := range oldTier1 {
		if _, exists := newTier1Map[heat.Path]; !exists {
			watchPath := filepath.Join(config.StartPath, heat.Path)
			if err := sw.Watcher.Remove(watchPath); err != nil {
				log.Warnf("Failed to remove path %s from watcher: %v", watchPath, err)
			}
		}
	}
}

func (sw *ScoreWatch) monitorTier2() {
	ticker := time.NewTicker(sw.tier2Interval)
	defer ticker.Stop()

	tier2Index := 0

	// tier2 会随 performScan 重建，为空时跳过本轮而不是退出，
	// 否则后续再有目录降级到 tier2 就没人轮询了
	for {
		select {
		case <-ticker.C:
			sw.mu.Lock()
			if len(sw.tier2) == 0 {
				sw.mu.Unlock()
				continue
			}
			if tier2Index >= len(sw.tier2) {
				tier2Index = 0
			}
			heat := sw.tier2[tier2Index]
			sw.mu.Unlock()
			tier2Index++

			if err := hasDirectoryChanged(heat.Path); err != nil {
				log.Warnf("Failed to check directory change for %s: %v", heat.Path, err)
			}

		case <-sw.ctx.Done():
			return
		}
	}
}

func hasDirectoryChanged(path string) error {
	oldContents, err := tree.GetDirContents(path)
	fullPath := filepath.Join(config.StartPath, path)

	if err != nil {
		return fmt.Errorf("failed to get directory contents: %w", err)
	}
	oldNodes := make(map[string]tree.Node, len(oldContents))
	for _, node := range oldContents {
		oldNodes[node.Path] = node
	}
	entries, err := os.ReadDir(fullPath)
	if err != nil {
		return nil
	}
	newNodes := make(map[string]os.DirEntry, len(entries))
	for _, entry := range entries {
		nodePath := filepath.Join(path, entry.Name())
		newNodes[nodePath] = entry
		oldNode, exists := oldNodes[nodePath]
		if !exists {
			event := &fsnotify.Event{
				Name: filepath.Join(fullPath, entry.Name()),
				Op:   fsnotify.Create,
			}
			eventFilter(*event)
			continue
		}
		// 已存在的文件比较大小和修改时间，捕捉内容修改
		if !entry.IsDir() {
			if info, err := entry.Info(); err == nil &&
				(uint64(info.Size()) != oldNode.Size || !info.ModTime().Equal(oldNode.ModTime)) {
				event := &fsnotify.Event{
					Name: filepath.Join(fullPath, entry.Name()),
					Op:   fsnotify.Write,
				}
				eventFilter(*event)
			}
		}
	}
	for _, entry2 := range oldContents {
		nodePath := entry2.Path
		if _, exists := newNodes[nodePath]; !exists {
			event := &fsnotify.Event{
				Name: filepath.Join(fullPath, entry2.Name),
				Op:   fsnotify.Remove,
			}
			eventFilter(*event)
		}
	}
	return nil
}

func (sw *ScoreWatch) handleEvents() {
	log.Debug("ScoreWatch: Starting to handle events...")
	for {
		select {
		case event, ok := <-sw.Watcher.Events:
			if !ok {
				return
			}
			eventFilter(event)
		case err, ok := <-sw.Watcher.Errors:
			if !ok {
				return
			}
			log.Errorf("watcher error: %v", err)
		}
	}
}

func (sw *ScoreWatch) addHeat(path string, node *tree.Node) {
	score := sw.calculateInitScore(path, node)
	heatScore := &HeatScore{
		Path:       path,
		Deepth:     node.Depth,
		Score:      score,
		EventCount: 0,
	}

	sw.mu.Lock()
	sw.heatMap[path] = heatScore
	sw.tier1 = append(sw.tier1, heatScore)
	sw.mu.Unlock()

	if err := sw.Watcher.Add(filepath.Join(config.StartPath, path)); err != nil {
		log.Warnf("Failed to watch new directory %s: %v", path, err)
	}
}

// 删除
func (sw *ScoreWatch) removeHeat(path string) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if _, exists := sw.heatMap[path]; exists {
		delete(sw.heatMap, path)
		for i, tierHeat := range sw.tier1 {
			if tierHeat.Path == path {
				sw.tier1 = append(sw.tier1[:i], sw.tier1[i+1:]...)
				break
			}
		}
		for i, tierHeat := range sw.tier2 {
			if tierHeat.Path == path {
				sw.tier2 = append(sw.tier2[:i], sw.tier2[i+1:]...)
				break
			}
		}
	}
}
