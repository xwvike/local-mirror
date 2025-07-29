package diff

import (
	"local-mirror/internal/tree"
)

type Result struct {
	Path     string `json:"path"`
	IsDir    bool   `json:"is_dir"` // 是否为目录
	Action   string `json:"action"` // "create", "delete", "modify"
	Name     string `json:"name"`
	Size     uint64 `json:"size"`      // 文件大小
	ParentID string `json:"parent_id"` // 父目录ID
}

// CompareTreeStructures 比较两个树结构，以a为基准
func CompareTreeStructures(a, b []tree.Node) []Result {
	var diffs []Result

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
			// A中存在但B中不存在，表示需要在B中创建
			diffs = append(diffs, Result{
				Path:     pathA,
				IsDir:    nodeA.IsDir,
				Action:   "create",
				Name:     nodeA.Name,
				Size:     nodeA.Size,
				ParentID: nodeA.ParentID,
			})
		} else {
			// 如果都存在，检查hash（仅针对文件）
			if !nodeA.IsDir {
				if nodeA.Hash != nodeB.Hash {
					// Hash不同，需要修改
					diffs = append(diffs, Result{
						Path:     pathA,
						IsDir:    nodeA.IsDir,
						Action:   "modify",
						Name:     nodeA.Name,
						Size:     nodeA.Size,
						ParentID: nodeA.ParentID,
					})
				}
			}
		}
	}

	// 查找B中有但A中没有的，表示需要在B中删除
	for _, nodeB := range b {
		pathB := nodeB.Path
		_, exists := aMap[pathB]
		if !exists {
			diffs = append(diffs, Result{
				Path:     pathB,
				IsDir:    nodeB.IsDir,
				Action:   "delete",
				Name:     nodeB.Name,
				Size:     nodeB.Size,
				ParentID: nodeB.ParentID,
			})
		}
	}

	return diffs
}
