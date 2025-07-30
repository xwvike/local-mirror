package app

import (
	"encoding/json"
	"fmt"
	"local-mirror/app/tree"
	"local-mirror/pkg/diff"
	"local-mirror/pkg/stack"
)

var (
	diffQueue = stack.NewStack[diff.DiffResult]()
)

func Diff(realityTree []byte, path string) error {
	localTree, err := tree.GetDirContents(path)
	if err != nil {
		return fmt.Errorf("failed to get local tree contents: %w", err)
	}
	var realityTreeData []tree.Node
	json.Unmarshal(realityTree, &realityTreeData)
	diffs := diff.FindDifferences(realityTreeData, localTree)
	for _, diff := range diffs {
		diffQueue.Push(diff)
	}
	return nil
}
