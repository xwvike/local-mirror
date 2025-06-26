package app

import (
	"encoding/json"
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
		log.Error("Error getting local tree contents:", err)
		return err
	}
	var realityTreeData []tree.Node
	json.Unmarshal(realityTree, &realityTreeData)
	diffs := jsonutil.FindDifferences(realityTreeData, localTree)
	for _, diff := range diffs {
		diffQueue.Push(diff)
	}
	return nil
}
