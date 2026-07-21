package app

import (
	"fmt"
	"local-mirror/internal/tree"
	"time"
)

type DiffResult struct {
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"` // 是否为目录
	Action  string    `json:"action"` // "create", "delete", "modify"
	Name    string    `json:"name"`
	Size    uint64    `json:"size"`     // 文件大小
	Hash    string    `json:"hash"`     // 文件内容哈希（create/modify 取服务端，delete 取本地）
	ModTime time.Time `json:"mod_time"` // 源文件修改时间，用于镜像端保真
}

// FindDifferences 比较两个树结构，以 a（服务端）为基准
func FindDifferences(a, b []tree.Node) []DiffResult {
	var diffs []DiffResult

	// 将b转换为map以便快速查找
	bMap := make(map[string]tree.Node)
	aMap := make(map[string]tree.Node)
	for _, node := range b {
		bMap[node.Path] = node
	}

	for _, nodeA := range a {
		aMap[nodeA.Path] = nodeA
		nodeB, exists := bMap[nodeA.Path]
		if !exists {
			diffs = append(diffs, DiffResult{
				Path:    nodeA.Path,
				IsDir:   nodeA.IsDir,
				Action:  "create",
				Name:    nodeA.Name,
				Size:    nodeA.Size,
				Hash:    nodeA.Hash,
				ModTime: nodeA.ModTime,
			})
			continue
		}
		// 大小不同肯定变了；哈希仅在两侧都算出来时才可比
		if nodeA.Size != nodeB.Size ||
			(nodeA.Hash != "" && nodeB.Hash != "" && nodeA.Hash != nodeB.Hash) {
			diffs = append(diffs, DiffResult{
				Path:    nodeA.Path,
				IsDir:   nodeA.IsDir,
				Action:  "modify",
				Name:    nodeA.Name,
				Size:    nodeA.Size,
				Hash:    nodeA.Hash,
				ModTime: nodeA.ModTime,
			})
		}
	}
	for _, nodeB := range b {
		if _, exists := aMap[nodeB.Path]; !exists {
			// 如果a中没有对应节点，标记为delete；哈希取本地节点，供移动检测配对
			diffs = append(diffs, DiffResult{
				Path:    nodeB.Path,
				IsDir:   nodeB.IsDir,
				Action:  "delete",
				Name:    nodeB.Name,
				Size:    nodeB.Size,
				Hash:    nodeB.Hash,
				ModTime: nodeB.ModTime,
			})
		}
	}

	return diffs
}

// Diff 用服务端目录列表与本地数据库中的同名目录比对，返回差异列表
func Diff(realityNodes []tree.Node, path string) ([]DiffResult, error) {
	localTree, err := tree.GetDirContents(path)
	if err != nil {
		return nil, fmt.Errorf("failed to get local tree contents: %w", err)
	}
	return FindDifferences(realityNodes, localTree), nil
}
