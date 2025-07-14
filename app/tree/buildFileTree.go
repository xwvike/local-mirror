package tree

import (
	"local-mirror/app/model"
	"local-mirror/common/utils"
	"local-mirror/config"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

func getLeafInfo(filepath string) *Node {
	fileInfo, err := os.Stat(filepath)
	var ignore = false
	for _, v := range model.IgnoreFileList {
		if strings.Contains(filepath, v) {
			ignore = true
			break
		}
	}
	if ignore {
		return nil
	}
	if err != nil {
		return nil
	}
	uuid, _ := utils.RandomString(16)
	return &Node{
		ID:       uuid,
		Path:     strings.Replace(filepath, config.StartPath, ".", 1),
		Name:     fileInfo.Name(),
		ParentID: "",
		IsDir:    fileInfo.IsDir(),
		Size:     uint64(fileInfo.Size()),
		ModTime:  fileInfo.ModTime(),
		Hash:     "",
	}
}

func BuildFileTreeTwoPhase(path string) error {
	startTime := time.Now().UnixMilli()
	log.Info("setp 1 >> start build file tree from path:", path)

	rootNode := getLeafInfo(path)
	if rootNode == nil {
		log.Error("Failed to get root node info, path may not exist:", path)
		os.Exit(1)
		return nil
	}
	if !rootNode.IsDir {
		log.Error("The specified path is not a directory:", path)
		os.Exit(1)
		return nil
	}

	// 第一阶段：收集所有目录
	dirArray := make([]*Node, 0)
	dirArray = append(dirArray, rootNode)
	var collectDirs func(string, *Node)
	collectDirs = func(dirPath string, parent *Node) {
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			return
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			childPath := filepath.Join(dirPath, entry.Name())
			childNode := getLeafInfo(childPath)
			if childNode == nil {
				continue
			}
			ignore := false

			for _, v := range model.IgnoreFileList {
				if strings.Contains(childNode.Path, v) {
					ignore = true
					break
				}
			}
			// 跳过被忽略的目录
			if ignore {
				continue
			}

			childNode.ParentID = parent.ID
			dirArray = append(dirArray, childNode)

			collectDirs(childPath, childNode)
		}
	}

	collectDirs(path, rootNode)
	log.Infof("Phase 1 completed: collected %d directories", len(dirArray))
	AddNodes(dirArray)
	// 第二阶段：为每个目录添加文件
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allFileNodes = make([]*Node, 0)
	var jobs = make(chan *Node, 100)

	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		go func(WorkerID int) {
			defer wg.Done()
			for node := range jobs {
				log.Debugf("Worker %d processing directory: %s", WorkerID, node.Path)
				files := processDirectory(node)
				if files == nil {
					continue
				}
				if len(files) > 0 {
					mu.Lock()
					allFileNodes = append(allFileNodes, files...)
					if len(allFileNodes) >= 1000 {
						AddNodes(allFileNodes)
						allFileNodes = allFileNodes[:0]
					}
					mu.Unlock()
				}
			}
			log.Infof("Worker %d finished processing directories", WorkerID)
		}(i)
	}

	go func() {
		defer close(jobs)
		processed := 0
		for _, dirNode := range dirArray {
			jobs <- dirNode
			processed++
			if processed%1000 == 0 {
				log.Infof("Queued %d/%d directories", processed, len(dirArray))
			}
		}
	}()

	wg.Wait()
	AddNodes(allFileNodes)

	log.Infof("file tree build completed, time taken: %d ms", time.Now().UnixMilli()-startTime)
	time.Sleep(300 * time.Millisecond)
	fileCount, _ := GetMeta("file_count")
	log.Info("file tree build completed all files count:", fileCount)
	return nil
}

func processDirectory(node *Node) []*Node {
	path := node.Path
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsPermission(err) {
			log.Warnf("Permission denied reading directory: %s", path)
		} else {
			log.Warnf("Error reading directory %s: %v", path, err)
		}
		return nil
	}

	files := make([]*Node, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		childPath := filepath.Join(config.StartPath, path, entry.Name())
		childNode := getLeafInfo(childPath)
		if childNode == nil {
			continue
		}

		ignore := false
		for _, v := range model.IgnoreFileList {
			if strings.Contains(childNode.Path, v) {
				ignore = true
				break
			}
		}

		if ignore {
			continue
		}

		childNode.ParentID = node.ID
		files = append(files, childNode)
	}

	return files
}
