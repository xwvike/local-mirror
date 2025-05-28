package app

import (
	"encoding/json"
	log "github.com/sirupsen/logrus"
	"local-mirror/pkg/jsonutil"
)

var (
	// ErrLeafNotFound is returned when a leaf is not found in the tree.
	diffQueue = []jsonutil.DiffResult{}
)

func Diff(a map[string]interface{}, leaf *Leaf) error {
	LeafBytes, err := json.Marshal(leaf)
	if err != nil {
		log.Error("Error marshaling leaf:", err)
		return err
	}
	aBytes, err := json.Marshal(a)
	if err != nil {
		log.Error("Error marshaling tree response data:", err)
		return err
	}
	diffs, err := jsonutil.FindDifferencesFromJSON(string(aBytes), string(LeafBytes))
	if err != nil {
		log.Error("Error finding differences:", err)
		return err
	}
	diffQueue = append(diffQueue, diffs...)
	diffList := ""
	for _, diff := range diffQueue {
		diffList += diff.Name + "-----" + diff.Path + "-----" + diff.Type + "-----" + diff.Action + "/\n"
	}
	log.Infof("Differences found: /\n %s", diffList)
	return nil
}
