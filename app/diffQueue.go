package app

import (
	"encoding/json"
	"fmt"
	"local-mirror/app/tree"
	"local-mirror/common/data"
	"local-mirror/common/jsonutil"

	log "github.com/sirupsen/logrus"
)

var (
	diffQueue = data.NewStack[jsonutil.DiffResult]()
)

func Diff(realityTree []byte, path string) error {
	localTree, err := tree.GetDirContents(path)
	if err != nil {
		return fmt.Errorf("failed to get local tree contents: %w", err)
	}
	var realityTreeData []tree.Node
	json.Unmarshal(realityTree, &realityTreeData)
	diffs := jsonutil.FindDifferences(realityTreeData, localTree)
	for _, diff := range diffs {
		diffQueue.Push(diff)
	}
	return nil
}
