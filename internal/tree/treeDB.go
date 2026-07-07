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
5. changed_dirs: 存储目录变动信息，支持按时间范围查询
   - key: unix秒时间戳
   - value: 目录路径数组
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

// SchemaVersion 数据库结构版本。节点序列化格式或桶结构变化时递增，
// 旧版本缓存直接重建，避免读到不兼容的数据
const SchemaVersion = "1"

var allBuckets = []string{"nodes", "children", "path_index", "meta", "changed_dirs"}

func InitDB() {
	var err error
	// 必须设置 Timeout：bbolt 依赖文件锁，同一目录再启动一个实例时
	// 不带超时的 Open 会无限期阻塞，进程看起来像卡死
	DB, err = bolt.Open("./.local-mirror/cache.db", 0600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		log.Errorf("打开数据库失败（该目录可能已有另一个 local-mirror 实例在运行）: %v", err)
		os.Exit(1)
	}

	if err = DB.Update(func(tx *bolt.Tx) error {
		// 缓存可复用的条件：同步根目录一致 + 结构版本一致 + 所有桶齐全。
		// 满足则跨重启复用，BuildFileTree 只需校准增量，省掉全量重哈希
		reuse := true
		if meta := tx.Bucket([]byte("meta")); meta == nil ||
			string(meta.Get([]byte("start_path"))) != config.StartPath ||
			string(meta.Get([]byte("schema_version"))) != SchemaVersion {
			reuse = false
		}
		for _, name := range allBuckets {
			if tx.Bucket([]byte(name)) == nil {
				reuse = false
			}
		}

		if reuse {
			log.Info("复用上次运行的目录树缓存")
			// 变更记录属于上一个实例的时间线：服务端重启后 InstanceID 变化，
			// 客户端会重建会话并全量扫描，旧记录只会造成误导，直接清空
			if err := tx.DeleteBucket([]byte("changed_dirs")); err != nil {
				return fmt.Errorf("failed to reset changed_dirs: %w", err)
			}
			if _, err := tx.CreateBucket([]byte("changed_dirs")); err != nil {
				return fmt.Errorf("failed to recreate changed_dirs: %w", err)
			}
		} else {
			log.Info("缓存不可复用（首次运行/目录变化/结构升级），重建数据库")
			for _, name := range allBuckets {
				if tx.Bucket([]byte(name)) != nil {
					if err := tx.DeleteBucket([]byte(name)); err != nil {
						return fmt.Errorf("failed to delete bucket %s: %w", name, err)
					}
				}
				if _, err := tx.CreateBucket([]byte(name)); err != nil {
					return fmt.Errorf("failed to create bucket %s: %w", name, err)
				}
			}
			metaBucket := tx.Bucket([]byte("meta"))
			zero := make([]byte, 8)
			if err := metaBucket.Put([]byte("dir_count"), zero); err != nil {
				return err
			}
			if err := metaBucket.Put([]byte("file_count"), zero); err != nil {
				return err
			}
		}

		metaBucket := tx.Bucket([]byte("meta"))
		if err := metaBucket.Put([]byte("start_path"), []byte(config.StartPath)); err != nil {
			return err
		}
		if err := metaBucket.Put([]byte("schema_version"), []byte(SchemaVersion)); err != nil {
			return err
		}
		return metaBucket.Put([]byte("start_time"), []byte(time.Now().Format(time.RFC3339)))
	}); err != nil {
		log.Error("bbolt: failed to initialize database:", err)
		os.Exit(1)
	}
	log.Info("Database initialized successfully")
}

// LoadAllNodesByPath 把 nodes 桶整体加载为 path → Node 映射，
// 供 BuildFileTree 启动校准时复用节点 ID 与哈希
func LoadAllNodesByPath() (map[string]*Node, error) {
	nodes := make(map[string]*Node)
	err := DB.View(func(tx *bolt.Tx) error {
		nodesBucket := tx.Bucket([]byte("nodes"))
		if nodesBucket == nil {
			return fmt.Errorf("nodes bucket not found")
		}
		return nodesBucket.ForEach(func(k, v []byte) error {
			var node Node
			if err := json.Unmarshal(v, &node); err != nil {
				return err
			}
			nodes[node.Path] = &node
			return nil
		})
	})
	return nodes, err
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
	// Go 命名规范：驼峰式，不用下划线
	var dirCount uint64
	var fileCount uint64
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

			// 同一路径重复写入视为更新：复用已有节点ID并保留父子关系，
			// 避免 nodes 桶残留孤儿节点、children 列表出现重复引用
			if existingID := pathIndexBucket.Get([]byte(node.Path)); existingID != nil {
				node.ID = string(existingID)
				if oldData := nodesBucket.Get(existingID); oldData != nil {
					var old Node
					if err := json.Unmarshal(oldData, &old); err == nil && old.ParentID != "" {
						node.ParentID = old.ParentID
					}
				}
				nodeData, err := json.Marshal(*node)
				if err != nil {
					log.Error("Failed to marshal node:", err)
					return err
				}
				if err := nodesBucket.Put(existingID, nodeData); err != nil {
					return err
				}
				continue
			}

			nodeData, err := json.Marshal(*node)
			if err != nil {
				log.Error("Failed to marshal node:", err)
				return err
			}
			if err := nodesBucket.Put([]byte(node.ID), nodeData); err != nil {
				return err
			}
			if err := pathIndexBucket.Put([]byte(node.Path), []byte(node.ID)); err != nil {
				return err
			}
			// bool 类型直接用 if/else，switch bool 在 Go 中不惯用
			if node.IsDir {
				dirCount++
			} else {
				fileCount++
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
				if err := childrenBucket.Put([]byte(node.ParentID), childrenData); err != nil {
					return err
				}
			}
			log.Debugf("Node %s added successfully", node.ID)
		}
		if dirCount > 0 {
			oldDirCount := metaBucket.Get([]byte("dir_count"))
			if oldDirCount != nil {
				newDirCount := binary.BigEndian.Uint64(oldDirCount) + dirCount
				dirCountData := make([]byte, 8)
				binary.BigEndian.PutUint64(dirCountData, newDirCount)
				if err := metaBucket.Put([]byte("dir_count"), dirCountData); err != nil {
					log.Error("Failed to update directory count:", err)
					return err
				}
			} else {
				dirCountData := make([]byte, 8)
				binary.BigEndian.PutUint64(dirCountData, dirCount)
				if err := metaBucket.Put([]byte("dir_count"), dirCountData); err != nil {
					log.Error("Failed to set initial directory count:", err)
					return err
				}
			}
		}
		if fileCount > 0 {
			oldFileCount := metaBucket.Get([]byte("file_count"))
			if oldFileCount != nil {
				newFileCount := binary.BigEndian.Uint64(oldFileCount) + fileCount
				fileCountData := make([]byte, 8)
				binary.BigEndian.PutUint64(fileCountData, newFileCount)
				if err := metaBucket.Put([]byte("file_count"), fileCountData); err != nil {
					log.Error("Failed to update file count:", err)
					return err
				}
			} else {
				fileCountData := make([]byte, 8)
				binary.BigEndian.PutUint64(fileCountData, fileCount)
				if err := metaBucket.Put([]byte("file_count"), fileCountData); err != nil {
					log.Error("Failed to set initial file count:", err)
					return err
				}
			}
		}
		return nil
	})
	log.Debugf("Added %d directories and %d files to the database", dirCount, fileCount)
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
		// 直接将比较结果赋值，无需 if/else
		exists = pathIndexBucket.Get([]byte(path)) != nil
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

func GetAllDirNodes() ([]*Node, error) {
	var dirNodes []*Node
	err := DB.View(func(tx *bolt.Tx) error {
		nodesBucket := tx.Bucket([]byte("nodes"))
		if nodesBucket == nil {
			return fmt.Errorf("nodes bucket not found")
		}
		return nodesBucket.ForEach(func(k, v []byte) error {
			var node Node
			if err := json.Unmarshal(v, &node); err != nil {
				return err
			}
			if node.IsDir {
				dirNodes = append(dirNodes, &node)
			}
			return nil
		})
	})
	return dirNodes, err
}

// addChangedDir 把一批变更目录写入 changed_dirs 桶。
// 必须以落库时刻做 key：若用事件发生时间，节流延迟会让记录"出现在过去"，
// 客户端按时间窗游标查询时正好跳过它们
func addChangedDir(paths []string) error {
	err := DB.Update(func(tx *bolt.Tx) error {
		changedDirsBucket := tx.Bucket([]byte("changed_dirs"))
		if changedDirsBucket == nil {
			log.Error("Database buckets not initialized")
			return os.ErrNotExist
		}

		oneHourAgo := uint64(time.Now().Add(-1 * time.Hour).Unix())
		c := changedDirsBucket.Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			if binary.BigEndian.Uint64(k) < oneHourAgo {
				if err := changedDirsBucket.Delete(k); err != nil {
					log.Warnf("Failed to delete old changed dir record: %v", err)
				}
			} else {
				break
			}
		}

		timeStampBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(timeStampBytes, uint64(time.Now().Unix()))

		// 同一秒内可能已有记录，合并而不是覆盖
		if existing := changedDirsBucket.Get(timeStampBytes); existing != nil {
			var existingPaths []string
			if err := json.Unmarshal(existing, &existingPaths); err == nil {
				paths = append(existingPaths, paths...)
			}
		}

		dirData, err := json.Marshal(paths)
		if err != nil {
			return fmt.Errorf("failed to marshal changed dirs: %w", err)
		}
		return changedDirsBucket.Put(timeStampBytes, dirData)
	})
	return err
}

func GetChangedDirs(start int64, end int64) ([]string, error) {
	var dirs []string
	err := DB.View(func(tx *bolt.Tx) error {
		changedDirsBucket := tx.Bucket([]byte("changed_dirs"))
		if changedDirsBucket == nil {
			return fmt.Errorf("changed_dirs bucket not found")
		}
		c := changedDirsBucket.Cursor()
		startStamp := uint64(start)
		endStamp := uint64(end)
		startStampBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(startStampBytes, startStamp)

		for k, v := c.Seek(startStampBytes); k != nil && binary.BigEndian.Uint64(k) <= endStamp; k, v = c.Next() {
			var _dirs []string
			if err := json.Unmarshal(v, &_dirs); err != nil {
				log.Error("Failed to unmarshal changed dir:", err)
				continue
			}
			dirs = append(dirs, _dirs...)
		}
		return nil
	})
	return dirs, err
}
