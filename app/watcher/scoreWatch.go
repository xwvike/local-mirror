package watcher

import (
	"context"
	"fmt"
	"local-mirror/app/tree"
	"local-mirror/config"
	"local-mirror/pkg/utils"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

type ScoreWatch struct {
	Watcher         *fsnotify.Watcher
	maxfilesperproc uint16 //https://pkg.go.dev/github.com/fsnotify/fsnotify@v1.8.0#readme-linux
	tier1Limit      uint16
	tier2Interval   time.Duration
	heatMap         map[string]*HeatScore
	tier1           []*HeatScore
	tier2           []*HeatScore
	ctx             context.Context
	cancel          context.CancelFunc
}

type HeatScore struct {
	Path       string
	Deepth     int
	Score      float64
	EventCount int
}

var GlobalScoreWatch *ScoreWatch

func InitWatcher(watcher *fsnotify.Watcher) error {
	var _maxWatches uint16 = 10240
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
		log.Warnf("Unsupported OS %s, using default max watchers value", osType)
		_maxWatches = 1024
	}
	ctx, cancel := context.WithCancel(context.Background())
	GlobalScoreWatch = &ScoreWatch{
		Watcher:         watcher,
		maxfilesperproc: _maxWatches,
		tier1Limit:      4,
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

	for _, dir := range allDir {
		path := dir.Path
		score := sw.calculateInitScore(path, dir)
		fmt.Println("Initial score for path:", path, "is", score)
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
	fileCount, err := os.ReadDir(path)
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
	dirs := make([]*HeatScore, 0)
	for _, heat := range sw.heatMap {
		dirs = append(dirs, heat)
	}

	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].Score > dirs[j].Score
	})
	usedWatches := uint16(0)
	oldTier1 := sw.tier1
	for i, heat := range dirs {
		fileCount, err := os.ReadDir(heat.Path)
		if err != nil {
			log.Warnf("Failed to read directory %s: %v", heat.Path, err)
			continue
		}
		fileCountInt := uint16(len(fileCount))
		switch utils.BaseOSInfo().OS {
		case "darwin":
			usedWatches += fileCountInt + 1
		case "linux":
			usedWatches++
		case "windows":
			usedWatches++
		default:
			log.Warnf("Unsupported OS %s, cannot determine used watches", utils.BaseOSInfo().OS)
		}
		if usedWatches < sw.tier1Limit {
			sw.Watcher.Add(strings.Replace(heat.Path, ".", config.StartPath, 1))
		} else {
			sw.tier1 = dirs[:i]
			dirs = dirs[i:]
			sw.tier2 = append(sw.tier2, dirs...)
			break
		}
	}
	sw.removeOldWatchers(oldTier1, sw.tier1)
}

func (sw *ScoreWatch) removeOldWatchers(oldTier1, newTier1 []*HeatScore) {
	newTier1Map := make(map[string]struct{})
	for _, heat := range newTier1 {
		newTier1Map[heat.Path] = struct{}{}
	}

	for _, heat := range oldTier1 {
		if _, exists := newTier1Map[heat.Path]; !exists {
			watchPath := strings.Replace(heat.Path, ".", config.StartPath, 1)
			if err := sw.Watcher.Remove(watchPath); err != nil {
				log.Warnf("Failed to remove path %s from watcher: %v", watchPath, err)
			}
		}
	}
}

func (sw *ScoreWatch) monitorTier2() {
	if len(sw.tier2) == 0 {
		return
	}

	ticker := time.NewTicker(sw.tier2Interval)
	defer ticker.Stop()

	tier2Index := 0

	for {
		select {
		case <-ticker.C:
			if len(sw.tier2) == 0 {
				return
			}

			if tier2Index >= len(sw.tier2) {
				tier2Index = 0
			}

			heat := sw.tier2[tier2Index]
			tier2Index++

			err := hasDirectoryChanged(heat.Path)
			if err != nil {
				log.Warnf("Failed to check directory change for %s: %v", heat.Path, err)
			}

		case <-sw.ctx.Done():
			return
		}
	}
}

func hasDirectoryChanged(path string) error {
	oldContents, err := tree.GetDirContents(path)
	fullPath := strings.Replace(path, ".", config.StartPath, 1)

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
		nodePath := path + string(os.PathSeparator) + entry.Name()
		newNodes[nodePath] = entry
		if _, exists := oldNodes[nodePath]; !exists {
			event := &fsnotify.Event{
				Name: fullPath + string(os.PathSeparator) + entry.Name(),
				Op:   fsnotify.Create,
			}
			eventFilter(*event)
		}
	}
	for _, entry2 := range oldContents {
		nodePath := entry2.Path
		if _, exists := newNodes[nodePath]; !exists {
			event := &fsnotify.Event{
				Name: fullPath + string(os.PathSeparator) + entry2.Name,
				Op:   fsnotify.Remove,
			}
			eventFilter(*event)
		}
	}
	return nil
}

func (sw *ScoreWatch) handleEvents() {
	fmt.Println("ScoreWatch: Starting to handle events...")
	fmt.Println(sw.Watcher.WatchList())
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
	fmt.Println("Adding heat for path:", path, "with score:", score)
	heatScore := &HeatScore{
		Path:       path,
		Deepth:     node.Depth,
		Score:      score,
		EventCount: 0,
	}
	sw.heatMap[path] = heatScore
	sw.tier1 = append(sw.tier1, heatScore)
	sw.Watcher.Add(strings.Replace(path, ".", config.StartPath, 1))
}

// 删除
func (sw *ScoreWatch) removeHeat(path string) {
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
