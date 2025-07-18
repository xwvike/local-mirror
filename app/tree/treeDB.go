package tree

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"local-mirror/config"
	"os"

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
}

type Children struct {
	ParentID string   `json:"parent_id"`
	ChildIDs []string `json:"child_ids"` // 子节点ID列表
}

func InitDB() {
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

func AddNodes(nodes []*Node) error {
	log.Debug("Adding nodes to the database:", len(nodes))
	var dir_count uint64 = 0
	var file_count uint64 = 0
	_err := DB.Update(func(tx *bolt.Tx) error {
		nodesBucket := tx.Bucket([]byte("nodes"))
		childrenBucket := tx.Bucket([]byte("children"))
		pathIndexBucket := tx.Bucket([]byte("path_index"))
		metaBucket := tx.Bucket([]byte("meta"))
		if nodesBucket == nil || childrenBucket == nil || pathIndexBucket == nil || metaBucket == nil {
			log.Error("Database buckets not initialized")
			return os.ErrNotExist // 确保所有必要的桶都存在
		}
		for _, node := range nodes {
			log.Debugf("Adding node: %s, Path: %s, ParentID: %s", node.ID, node.Path, node.ParentID)
			nodeData, err := json.Marshal(*node)
			if err != nil {
				log.Error("Failed to marshal node:", err)
				return err
			}
			nodesBucket.Put([]byte(node.ID), nodeData)
			pathIndexBucket.Put([]byte(node.Path), []byte(node.ID))
			switch node.IsDir {
			case true:
				dir_count++
			case false:
				file_count++
			}
			if node.ParentID != "" {
				childrenData := childrenBucket.Get([]byte(node.ParentID))
				var children Children
				if childrenData != nil {
					if err := json.Unmarshal(childrenData, &children); err != nil {
						return err
					}
				} else {
					children = Children{ParentID: node.ParentID, ChildIDs: []string{}}
				}
				children.ChildIDs = append(children.ChildIDs, node.ID)
				childrenData, err = json.Marshal(children)
				if err != nil {
					return err
				}
				childrenBucket.Put([]byte(node.ParentID), childrenData)
			}
			log.Debugf("Node %s added successfully", node.ID)
		}
		if dir_count > 0 {
			oldDirCount := metaBucket.Get([]byte("dir_count"))
			if oldDirCount != nil {
				newDirCount := binary.BigEndian.Uint64(oldDirCount) + dir_count
				dirCountData := make([]byte, 8)
				binary.BigEndian.PutUint64(dirCountData, newDirCount)
				if err := metaBucket.Put([]byte("dir_count"), dirCountData); err != nil {
					log.Error("Failed to update directory count:", err)
					return err
				}
			} else {
				dirCountData := make([]byte, 8)
				binary.BigEndian.PutUint64(dirCountData, dir_count)
				if err := metaBucket.Put([]byte("dir_count"), dirCountData); err != nil {
					log.Error("Failed to set initial directory count:", err)
					return err
				}
			}
		}
		if file_count > 0 {
			oldFileCount := metaBucket.Get([]byte("file_count"))
			if oldFileCount != nil {
				newFileCount := binary.BigEndian.Uint64(oldFileCount) + file_count
				fileCountData := make([]byte, 8)
				binary.BigEndian.PutUint64(fileCountData, newFileCount)
				if err := metaBucket.Put([]byte("file_count"), fileCountData); err != nil {
					log.Error("Failed to update file count:", err)
					return err
				}
			} else {
				fileCountData := make([]byte, 8)
				binary.BigEndian.PutUint64(fileCountData, file_count)
				if err := metaBucket.Put([]byte("file_count"), fileCountData); err != nil {
					log.Error("Failed to set initial file count:", err)
					return err
				}
			}
		}
		return nil
	})
	log.Debugf("Added %d directories and %d files to the database", dir_count, file_count)
	return _err
}

func DeleteNodes(nodePaths []string) error {
	log.Debug("Deleting nodes:", len(nodePaths))
	if len(nodePaths) == 0 {
		return nil
	}

	return DB.Update(func(tx *bolt.Tx) error {
		nodesBucket := tx.Bucket([]byte("nodes"))
		childrenBucket := tx.Bucket([]byte("children"))
		pathIndexBucket := tx.Bucket([]byte("path_index"))
		metaBucket := tx.Bucket([]byte("meta"))

		if nodesBucket == nil || childrenBucket == nil || pathIndexBucket == nil || metaBucket == nil {
			log.Error("Database buckets not initialized")
			return os.ErrNotExist
		}

		var totalDirCount, totalFileCount uint64
		var allNodesToDelete []string
		var parentUpdates = make(map[string][]string) // parentID -> childIDs to remove

		// 预收集所有需要删除的节点信息
		for _, nodePath := range nodePaths {
			// 获取要删除的节点ID
			nodeID := pathIndexBucket.Get([]byte(nodePath))
			if nodeID == nil {
				log.Warnf("node not found: %s", nodePath)
				continue
			}

			// 获取节点信息
			nodeData := nodesBucket.Get(nodeID)
			if nodeData == nil {
				log.Warnf("node data not found for ID: %s", string(nodeID))
				continue
			}

			var rootNode Node
			if err := json.Unmarshal(nodeData, &rootNode); err != nil {
				return err
			}

			// 收集父节点更新信息
			if rootNode.ParentID != "" {
				if _, exists := parentUpdates[rootNode.ParentID]; !exists {
					parentUpdates[rootNode.ParentID] = []string{}
				}
				parentUpdates[rootNode.ParentID] = append(parentUpdates[rootNode.ParentID], string(nodeID))
			}

			// 收集所有需要删除的节点（包括子节点）
			var nodesToDelete []string
			var dirCount, fileCount uint64

			if rootNode.IsDir {
				// 使用队列进行迭代遍历，收集所有需要删除的节点
				queue := []string{string(nodeID)}
				visited := make(map[string]bool)

				for len(queue) > 0 {
					currentID := queue[0]
					queue = queue[1:]

					if visited[currentID] {
						continue
					}
					visited[currentID] = true

					nodesToDelete = append(nodesToDelete, currentID)

					// 获取当前节点信息用于计数
					currentNodeData := nodesBucket.Get([]byte(currentID))
					if currentNodeData != nil {
						var currentNode Node
						if err := json.Unmarshal(currentNodeData, &currentNode); err != nil {
							return err
						}
						if currentNode.IsDir {
							dirCount++
						} else {
							fileCount++
						}
					}

					// 获取子节点
					childrenData := childrenBucket.Get([]byte(currentID))
					if childrenData != nil {
						var children Children
						if err := json.Unmarshal(childrenData, &children); err != nil {
							return err
						}
						// 将子节点加入队列
						queue = append(queue, children.ChildIDs...)
					}
				}
			} else {
				// 如果是文件，直接删除
				nodesToDelete = append(nodesToDelete, string(nodeID))
				fileCount = 1
			}

			allNodesToDelete = append(allNodesToDelete, nodesToDelete...)
			totalDirCount += dirCount
			totalFileCount += fileCount
		}

		if len(allNodesToDelete) == 0 {
			return nil
		}

		// 使用map去重，防止重复删除
		uniqueNodesToDelete := make(map[string]bool)
		for _, nodeID := range allNodesToDelete {
			uniqueNodesToDelete[nodeID] = true
		}

		// 批量删除所有收集到的节点
		for deleteID := range uniqueNodesToDelete {
			// 获取节点信息用于删除路径索引
			nodeData := nodesBucket.Get([]byte(deleteID))
			if nodeData != nil {
				var node Node
				if err := json.Unmarshal(nodeData, &node); err != nil {
					return err
				}
				// 删除路径索引
				pathIndexBucket.Delete([]byte(node.Path))
			}

			// 删除节点数据
			nodesBucket.Delete([]byte(deleteID))
			// 删除子节点关系
			childrenBucket.Delete([]byte(deleteID))
		}

		// 批量更新父节点信息
		for parentID, childIDsToRemove := range parentUpdates {
			parentChildrenData := childrenBucket.Get([]byte(parentID))
			if parentChildrenData != nil {
				var parentChildren Children
				if err := json.Unmarshal(parentChildrenData, &parentChildren); err != nil {
					return err
				}

				// 创建一个集合用于快速查找需要移除的子节点
				childIDSet := make(map[string]bool)
				for _, childID := range childIDsToRemove {
					childIDSet[childID] = true
				}

				// 过滤掉需要移除的子节点
				var newChildIDs []string
				for _, childID := range parentChildren.ChildIDs {
					if !childIDSet[childID] {
						newChildIDs = append(newChildIDs, childID)
					}
				}

				// 更新父节点的子节点列表
				if len(newChildIDs) > 0 {
					parentChildren.ChildIDs = newChildIDs
					updatedChildrenData, err := json.Marshal(parentChildren)
					if err != nil {
						return err
					}
					childrenBucket.Put([]byte(parentID), updatedChildrenData)
				} else {
					// 如果没有子节点了，删除整个记录
					childrenBucket.Delete([]byte(parentID))
				}
			}
		}

		// 更新计数
		if totalDirCount > 0 {
			oldDirCountData := metaBucket.Get([]byte("dir_count"))
			if oldDirCountData != nil {
				oldDirCount := binary.BigEndian.Uint64(oldDirCountData)
				if oldDirCount >= totalDirCount {
					newDirCount := oldDirCount - totalDirCount
					dirCountData := make([]byte, 8)
					binary.BigEndian.PutUint64(dirCountData, newDirCount)
					metaBucket.Put([]byte("dir_count"), dirCountData)
				}
			}
		}

		if totalFileCount > 0 {
			oldFileCountData := metaBucket.Get([]byte("file_count"))
			if oldFileCountData != nil {
				oldFileCount := binary.BigEndian.Uint64(oldFileCountData)
				if oldFileCount >= totalFileCount {
					newFileCount := oldFileCount - totalFileCount
					fileCountData := make([]byte, 8)
					binary.BigEndian.PutUint64(fileCountData, newFileCount)
					metaBucket.Put([]byte("file_count"), fileCountData)
				}
			}
		}

		log.Debugf("Deleted %d directories and %d files from the database", totalDirCount, totalFileCount)
		return nil
	})
}

// DeleteNode 保持向后兼容性
func DeleteNode(nodePath string) error {
	return DeleteNodes([]string{nodePath})
}

func HasPath(path string) (bool, error) {
	var exists bool
	err := DB.View(func(tx *bolt.Tx) error {
		pathIndexBucket := tx.Bucket([]byte("path_index"))
		if pathIndexBucket == nil {
			return fmt.Errorf("path index bucket not found")
		}
		pathID := pathIndexBucket.Get([]byte(path))
		if pathID != nil {
			exists = true
		} else {
			exists = false
		}
		return nil
	})
	return exists, err
}

func GetDirContents(dirPath string) ([]Node, error) {
	var contents = make([]Node, 0)
	return contents, DB.View(func(tx *bolt.Tx) error {
		nodesBucket := tx.Bucket([]byte("nodes"))
		childrenBucket := tx.Bucket([]byte("children"))
		pathIndexBucket := tx.Bucket([]byte("path_index"))

		pathID := string(pathIndexBucket.Get([]byte(dirPath)))
		if pathID == "" {
			return fmt.Errorf("directory not found: %s", dirPath)
		}
		childrenIds := childrenBucket.Get([]byte(pathID))
		if childrenIds == nil {
			return nil // 目录下没有子节点
		}
		var children Children
		if err := json.Unmarshal(childrenIds, &children); err != nil {
			return err
		}
		for _, childID := range children.ChildIDs {
			nodeData := nodesBucket.Get([]byte(childID))
			if nodeData == nil {
				continue // 跳过不存在的节点
			}
			var node Node
			if err := json.Unmarshal(nodeData, &node); err != nil {
				return err
			}
			contents = append(contents, node)
		}
		return nil
	})
}

func GetNodeByPath(path string) (*Node, error) {
	var node Node
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

		if err := json.Unmarshal(nodeData, &node); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &node, nil
}
