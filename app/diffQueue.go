package app

import (
	"encoding/json"
	"local-mirror/common/data"
	"local-mirror/common/jsonutil"

	log "github.com/sirupsen/logrus"
)

var (
	diffQueue = data.NewStack[jsonutil.DiffResult]()
)

func Diff(realityTree map[string]interface{}, leaf *Leaf) error {
	LeafBytes, err := json.Marshal(leaf)
	if err != nil {
		log.Error("Error marshaling leaf:", err)
		return err
	}
	aBytes, err := json.Marshal(realityTree)
	if err != nil {
		log.Error("Error marshaling tree response data:", err)
		return err
	}
	diffs, err := jsonutil.FindDifferencesFromJSON(string(aBytes), string(LeafBytes))
	if err != nil {
		log.Error("Error finding differences:", err)
		return err
	}
	for _, diff := range diffs {
		diffQueue.Push(diff)
	}
	return nil
}
