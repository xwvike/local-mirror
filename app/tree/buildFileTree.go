package tree

import (
	"local-mirror/app/model"
	"local-mirror/config"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

func getLeafInfo(filepath string) *model.Leaf {
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
	fileType := 0
	if fileInfo.IsDir() {
		fileType = 1
	}
	return &model.Leaf{
		Name:         fileInfo.Name(),
		Path:         filepath,
		RelativePath: strings.Replace(filepath, config.StartPath, ".", 1),
		Type:         uint8(fileType),
		Deep:         strings.Count(strings.TrimPrefix(filepath, config.StartPath), string(os.PathSeparator)),
		Size:         uint64(fileInfo.Size()),
		Children:     []*model.Leaf{},
	}
}

func BuildFileTreeTwoPhase(path string) *model.Leaf {
	startTime := time.Now().UnixMilli()
	log.Info("setp 1 >> start build file tree from path:", path)

	rootNode := getLeafInfo(path)
	if rootNode == nil {
		log.Error("Failed to get root node info, path may not exist:", path)
		return nil
	}
	if rootNode.Type != 1 {
		return rootNode
	}

	// 第一阶段：收集所有目录
	dirMap := make(map[string]*model.Leaf)
	dirMap[path] = rootNode

	var collectDirs func(string, *model.Leaf)
	collectDirs = func(dirPath string, parent *model.Leaf) {
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

			parent.Children = append(parent.Children, childNode)
			dirMap[childPath] = childNode

			collectDirs(childPath, childNode)
		}
	}

	collectDirs(path, rootNode)
	log.Infof("Phase 1 completed: collected %d directories", len(dirMap))

	// 第二阶段：为每个目录添加文件
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())

	for dirPath, dirNode := range dirMap {
		wg.Add(1)
		go func(path string, node *model.Leaf) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			entries, err := os.ReadDir(path)
			if err != nil {
				return
			}

			files := make([]*model.Leaf, 0)

			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}

				childPath := filepath.Join(path, entry.Name())
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

				files = append(files, childNode)
			}

			node.Children = append(node.Children, files...)
		}(dirPath, dirNode)
	}

	wg.Wait()

	log.Infof("file tree build completed, time taken: %d ms", time.Now().UnixMilli()-startTime)
	log.Info("file tree build completed all files count:", len(rootNode.GetAllFiles(999)))
	return rootNode
}
