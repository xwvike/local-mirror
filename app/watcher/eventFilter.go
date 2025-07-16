package watcher

import (
	"fmt"
	"local-mirror/app/tree"
	"local-mirror/common/utils"
	"local-mirror/config"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

var (
	create string = "CREATE"
	remove string = "REMOVE"
)

func eventFilter(event fsnotify.Event) {
	fmt.Printf("Event: %s\n", event)
	ignored := false
	for _, v := range config.IgnoreFileList {
		if strings.Contains(event.Name, v) {
			ignored = true
			break
		}
	}
	if ignored {
		return
	}
	nodeDir := strings.Replace(filepath.Dir(event.Name), config.StartPath, ".", 1)
	fatherNode, err := tree.GetNodeByPath(nodeDir)
	if err != nil {
		log.Errorf("Incomplete directory tree, unable to find parent node for %s: %v", nodeDir, err)
	} else {
		opStr := event.Op.String()
		osType := utils.BaseOSInfo().OS
		switch osType {
		case "darwin":
			create = "CREATE"
			remove = "REMOVE"
		case "linux":
			create = "CREATE"
			remove = "REMOVE"
		case "windows":

		default:

		}
		switch opStr {
		case create:
			fileInfo, err := os.Stat(event.Name)
			if err != nil {
				log.Error("Error getting file info:", err)
				return
			}
			uuid, _ := utils.RandomString(16)
			newLeaf := &tree.Node{
				ID:       uuid,
				Path:     strings.Replace(event.Name, config.StartPath, ".", 1),
				Name:     filepath.Base(event.Name),
				ParentID: fatherNode.ID,
				IsDir:    fileInfo.IsDir(),
				Size:     uint64(fileInfo.Size()),
				ModTime:  fileInfo.ModTime(),
				Hash:     "",
			}
			nodes := []*tree.Node{newLeaf}
			if err := tree.AddNodes(nodes); err != nil {
				log.Errorf("Failed to add node %s: %v", newLeaf.Name, err)
				return
			}
			log.Debugf("Added node %s to the tree", newLeaf.Name)
		case remove:
			err := tree.DeleteNode(event.Name)
			if err != nil {
				log.Errorf("Failed to delete node %s: %v", event.Name, err)
				return
			}
			log.Debugf("Deleted node %s from the tree", event.Name)
		}
	}
}
