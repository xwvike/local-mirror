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
	Size     int64     `json:"size"`
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
			tx.DeleteBucket([]byte(bucketName))
			if _, err := tx.CreateBucket([]byte(bucketName)); err != nil {
				return err
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
		log.Error("Failed to create database buckets:", err)
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
	error := DB.Update(func(tx *bolt.Tx) error {
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
	return error
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
