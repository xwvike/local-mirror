package tree

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/fs"
	"local-mirror/config"
	"local-mirror/pkg/utils"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
)

/*
数据库结构设计：
1. nodes: 存储所有节点信息
   - key: node ID (UUID)
   - value: JSON序列化的Node结构体
2. children: 存储每个目录的子节点ID列表
   - key: parent ID (目录ID)
   - value: JSON序列化的Children结构体
3. path_index: 存储路径到节点ID的映射
   - key: 完整路径 (Path)
   - value: 节点ID (UUID)
4. meta: 存储元数据，如文件和目录计数
   - key: 元数据键 (如 "file_count", "dir_count")
   - value: uint64类型的计数值
*/

var DB *bolt.DB

type Node struct {
	ID       string    `json:"id"`        // UUID
	Path     string    `json:"path"`      // 完整路径
	Name     string    `json:"name"`      // 文件/目录名
	ParentID string    `json:"parent_id"` // 父目录ID
	IsDir    bool      `json:"is_dir"`    // 是否为目录
	Size     uint64    `json:"size"`
	ModTime  time.Time `json:"mod_time"`
	Hash     string    `json:"hash"`
	Depth    int       `json:"depth"` // 目录深度
}

type Children struct {
	ParentID string   `json:"parent_id"`
	ChildIDs []string `json:"child_ids"` // 子节点ID列表
}

func InitializeDatabase() {
	var err error
	DB, err = bolt.Open(".local-mirror.db", 0600, nil)
	if err != nil {
		log.Error("Failed to open database:", err)
		os.Exit(1)
	}
	err = DB.Update(func(tx *bolt.Tx) error {
		buckets := []string{"nodes", "children", "path_index", "meta"}
		for _, bucketName := range buckets {
			// 删除旧的 bucket（如果存在）
			if tx.Bucket([]byte(bucketName)) != nil {
				if err := tx.DeleteBucket([]byte(bucketName)); err != nil {
					return fmt.Errorf("failed to delete bucket %s: %w", bucketName, err)
				}
			}
			// 创建新的空 bucket
			if _, err := tx.CreateBucket([]byte(bucketName)); err != nil {
				return fmt.Errorf("failed to create bucket %s: %w", bucketName, err)
			}
		}
		return nil
	})
	err = DB.Update(func(tx *bolt.Tx) error {
		metaBucket := tx.Bucket([]byte("meta"))
		// 初始化目录和文件计数
		dirCountData := make([]byte, 8)
		fileCountData := make([]byte, 8)
		binary.BigEndian.PutUint64(dirCountData, 0)
		binary.BigEndian.PutUint64(fileCountData, 0)
		metaBucket.Put([]byte("dir_count"), dirCountData)
		metaBucket.Put([]byte("file_count"), fileCountData)
		metaBucket.Put([]byte("start_path"), []byte(config.StartPath))
		metaBucket.Put([]byte("start_time"), []byte(time.Now().Format(time.RFC3339)))
		return nil
	})

	if err != nil {
		log.Error("bbolt:", err)
		os.Exit(1)
	}
	log.Info("Database initialized successfully")
}

func GetMeta(key string) (uint64, error) {
	var count uint64 = 0
	err := DB.View(func(tx *bolt.Tx) error {
		metaBucket := tx.Bucket([]byte("meta"))
		countData := metaBucket.Get([]byte(key))
		if countData != nil {
			count = binary.BigEndian.Uint64(countData)
		}
		return nil
	})
	return count, err
}

func BuildFileTree(path string) error {
	startTime := time.Now().UnixMilli()
	log.Info("start build file tree with concurrent WalkDir from path:", path)

	// 获取根节点信息
	rootInfo, err := os.Stat(path)
	if err != nil {
		log.Error("Failed to get root node info, path may not exist:", path)
		return err
	}
	if !rootInfo.IsDir() {
		log.Error("The specified path is not a directory:", path)
		return err
	}

	// 创建根节点
	uuid, _ := utils.GenerateRandomString(16)
	rootNode := &Node{
		ID:       uuid,
		Path:     ".",
		Name:     rootInfo.Name(),
		ParentID: "",
		IsDir:    true,
		Size:     uint64(rootInfo.Size()),
		ModTime:  rootInfo.ModTime(),
		Hash:     "",
		Depth:    0, // 根节点深度为0
	}

	// 用于存储路径到节点ID的映射
	pathToID := make(map[string]string)
	pathToID["."] = rootNode.ID

	// 使用并发安全的集合
	var allNodes []*Node
	var mu sync.Mutex

	// 使用工作池处理节点收集
	var workerCount = runtime.NumCPU()
	nodeChan := make(chan *Node, 1000)
	var wg sync.WaitGroup

	// 启动工作池
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for node := range nodeChan {
				mu.Lock()
				allNodes = append(allNodes, node)
				mu.Unlock()
			}
		}(i)
	}

	// 先添加根节点
	nodeChan <- rootNode

	// 使用WalkDir遍历
	walkErr := filepath.WalkDir(path, func(fullPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			log.Warnf("Error accessing path %s: %v", fullPath, walkErr)
			return nil
		}

		if fullPath == path {
			return nil
		}

		// 检查忽略列表
		relPath := strings.Replace(fullPath, config.StartPath, ".", 1)
		for _, ignorePattern := range config.IgnoreFileList {
			if strings.Contains(relPath, ignorePattern) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		// 获取文件信息
		info, err := d.Info()
		if err != nil {
			log.Warnf("Error getting file info for %s: %v", fullPath, err)
			return nil
		}

		// 创建节点
		uuid, _ := utils.GenerateRandomString(16)

		// 计算父节点路径
		parentPath := strings.Replace(filepath.Dir(fullPath), config.StartPath, ".", 1)

		// 获取父节点ID
		parentID := pathToID[parentPath]

		node := &Node{
			ID:       uuid,
			Path:     relPath,
			Name:     info.Name(),
			ParentID: parentID,
			IsDir:    info.IsDir(),
			Size:     uint64(info.Size()),
			ModTime:  info.ModTime(),
			Hash:     "",
			Depth:    strings.Count(relPath, string(filepath.Separator)),
		}

		// 记录路径到ID的映射
		pathToID[relPath] = uuid

		// 发送到工作池
		nodeChan <- node
		return nil
	})

	close(nodeChan)
	wg.Wait()

	if walkErr != nil {
		log.Error("Error walking directory:", walkErr)
		return walkErr
	}

	// 批量写入数据库
	log.Infof("Collected %d nodes with concurrent processing, writing to database...", len(allNodes))

	// 分批写入数据库
	batchSize := 1000
	for i := 0; i < len(allNodes); i += batchSize {
		end := i + batchSize
		if end > len(allNodes) {
			end = len(allNodes)
		}
		batch := allNodes[i:end]
		if err := AddNodes(batch); err != nil {
			log.Error("Error writing batch to database:", err)
			return err
		}
	}

	endTime := time.Now().UnixMilli()
	log.Infof("File tree build completed. Total time: %d ms, nodes: %d", endTime-startTime, len(allNodes))
	return nil
}

func AddNodes(nodes []*Node) error {
	return DB.Update(func(tx *bolt.Tx) error {
		nodesBucket := tx.Bucket([]byte("nodes"))
		childrenBucket := tx.Bucket([]byte("children"))
		pathIndexBucket := tx.Bucket([]byte("path_index"))

		childrenMap := make(map[string][]string)

		for _, node := range nodes {
			// 存储节点信息
			nodeData, err := json.Marshal(node)
			if err != nil {
				return fmt.Errorf("error marshaling node %s: %w", node.ID, err)
			}
			if err := nodesBucket.Put([]byte(node.ID), nodeData); err != nil {
				return fmt.Errorf("error storing node %s: %w", node.ID, err)
			}

			// 存储路径索引
			if err := pathIndexBucket.Put([]byte(node.Path), []byte(node.ID)); err != nil {
				return fmt.Errorf("error storing path index for %s: %w", node.Path, err)
			}

			// 构建父子关系
			if node.ParentID != "" {
				childrenMap[node.ParentID] = append(childrenMap[node.ParentID], node.ID)
			}
		}

		// 存储父子关系
		for parentID, childIDs := range childrenMap {
			children := Children{
				ParentID: parentID,
				ChildIDs: childIDs,
			}
			childrenData, err := json.Marshal(children)
			if err != nil {
				return fmt.Errorf("error marshaling children for parent %s: %w", parentID, err)
			}
			if err := childrenBucket.Put([]byte(parentID), childrenData); err != nil {
				return fmt.Errorf("error storing children for parent %s: %w", parentID, err)
			}
		}

		return nil
	})
}

func GetDirectoryContents(dirPath string) ([]Node, error) {
	var contents []Node
	return contents, DB.View(func(tx *bolt.Tx) error {
		pathIndexBucket := tx.Bucket([]byte("path_index"))
		nodesBucket := tx.Bucket([]byte("nodes"))

		// 查找目录节点ID
		dirNodeID := pathIndexBucket.Get([]byte(dirPath))
		if dirNodeID == nil {
			return fmt.Errorf("directory not found: %s", dirPath)
		}

		// 查找子节点
		childrenBucket := tx.Bucket([]byte("children"))
		childrenData := childrenBucket.Get(dirNodeID)
		if childrenData == nil {
			// 没有子节点，返回空列表
			return nil
		}

		var children Children
		if err := json.Unmarshal(childrenData, &children); err != nil {
			return fmt.Errorf("error unmarshaling children: %w", err)
		}

		// 获取所有子节点信息
		for _, childID := range children.ChildIDs {
			nodeData := nodesBucket.Get([]byte(childID))
			if nodeData != nil {
				var node Node
				if err := json.Unmarshal(nodeData, &node); err != nil {
					return err
				}
				contents = append(contents, node)
			}
		}
		return nil
	})
}

func HasPath(path string) (bool, error) {
	exists := false
	err := DB.View(func(tx *bolt.Tx) error {
		pathIndexBucket := tx.Bucket([]byte("path_index"))
		nodeID := pathIndexBucket.Get([]byte(path))
		exists = nodeID != nil
		return nil
	})
	return exists, err
}

func GetNodeByPath(path string) (*Node, error) {
	var node *Node
	err := DB.View(func(tx *bolt.Tx) error {
		pathIndexBucket := tx.Bucket([]byte("path_index"))
		nodesBucket := tx.Bucket([]byte("nodes"))

		nodeID := pathIndexBucket.Get([]byte(path))
		if nodeID == nil {
			return fmt.Errorf("node not found for path: %s", path)
		}

		nodeData := nodesBucket.Get(nodeID)
		if nodeData == nil {
			return fmt.Errorf("node data not found for ID: %s", string(nodeID))
		}

		node = &Node{}
		return json.Unmarshal(nodeData, node)
	})
	return node, err
}

func DeleteNode(path string) error {
	return DB.Update(func(tx *bolt.Tx) error {
		pathIndexBucket := tx.Bucket([]byte("path_index"))
		nodesBucket := tx.Bucket([]byte("nodes"))
		childrenBucket := tx.Bucket([]byte("children"))

		// 查找节点ID
		nodeID := pathIndexBucket.Get([]byte(path))
		if nodeID == nil {
			return fmt.Errorf("node not found for path: %s", path)
		}

		// 删除节点
		if err := nodesBucket.Delete(nodeID); err != nil {
			return err
		}

		// 删除路径索引
		if err := pathIndexBucket.Delete([]byte(path)); err != nil {
			return err
		}

		// 删除子节点关系（如果存在）
		if err := childrenBucket.Delete(nodeID); err != nil {
			return err
		}

		return nil
	})
}
