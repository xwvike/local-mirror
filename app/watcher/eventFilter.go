package watcher

import (
	"local-mirror/app/tree"
	"local-mirror/common/utils"
	"local-mirror/config"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

var (
	create string = "CREATE"
	remove string = "REMOVE"
)
var lastEventTime = make(map[string]time.Time)
var deleteEventCache []string

func eventFilter(event fsnotify.Event) {
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
		lastEventTime[opStr] = time.Now()
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
			deleteEventCache = append(deleteEventCache, event.Name)
			if time.Since(lastEventTime[opStr]) > 1*time.Second {
				err := tree.DeleteNodes(deleteEventCache)
				if err != nil {
					log.Errorf("Failed to delete node %s: %v", event.Name, err)
					return
				}
				log.Debugf("Deleted nodes count %d", len(deleteEventCache))
				deleteEventCache = slices.Delete(deleteEventCache, 0, len(deleteEventCache))
			}
		}
	}
}
