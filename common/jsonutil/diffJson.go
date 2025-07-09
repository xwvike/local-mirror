package jsonutil

import (
	"local-mirror/app/tree"
)

type DiffResult struct {
	Path   string `json:"path"`
	IsDir  bool   `json:"is_dir"` // 是否为目录
	Action string `json:"action"` // "create", "delete", "modify"
	Name   string `json:"name"`
	Size   uint64 `json:"size"` // 文件大小
}

// findDifferences 比较两个树结构，以a为基准
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
		pathA := nodeA.Path
		nodeB, exists := bMap[pathA]
		if !exists {
			// 如果b中没有对应节点，标记为add
			diffs = append(diffs, DiffResult{
				Path:   pathA,
				IsDir:  nodeA.IsDir,
				Action: "create",
				Name:   nodeA.Name,
				Size:   nodeA.Size,
			})
		}
		// 如果b中有对应节点，比较属性
		if exists {
			if nodeA.Size != nodeB.Size || nodeA.Hash != nodeB.Hash {
				// 如果属性不同，标记为modify
				diffs = append(diffs, DiffResult{
					Path:   pathA,
					IsDir:  nodeA.IsDir,
					Action: "modify",
					Name:   nodeA.Name,
					Size:   nodeA.Size,
				})
			}
		}
	}
	for _, nodeB := range b {
		pathB := nodeB.Path
		_, exists := aMap[pathB]
		if !exists {
			// 如果a中没有对应节点，标记为delete
			diffs = append(diffs, DiffResult{
				Path:   pathB,
				IsDir:  nodeB.IsDir,
				Action: "delete",
				Name:   nodeB.Name,
				Size:   nodeB.Size,
			})
		}
	}

	return diffs
}
