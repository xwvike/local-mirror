package watcher

import (
	"fmt"
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
var deleteEventCache []string
var deleteTimer *time.Timer
var deleteTimerActive bool

func eventFilter(event fsnotify.Event) {
	fmt.Println("eventFilter:", event.Name, "event.Op:", event.Op)
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
			if fileInfo.IsDir() {
				GlobalScoreWatch.addHeat(newLeaf.Path, newLeaf)
			}
			nodes := []*tree.Node{newLeaf}
			if err := tree.AddNodes(nodes); err != nil {
				log.Errorf("Failed to add node %s: %v", newLeaf.Name, err)
				return
			}
		case remove:
			deleteEventCache = append(deleteEventCache, strings.Replace(event.Name, config.StartPath, ".", 1))
			GlobalScoreWatch.removeHeat(strings.Replace(event.Name, config.StartPath, ".", 1))
			if deleteTimerActive {
				deleteTimer.Stop()
			}

			deleteTimer = time.AfterFunc(1*time.Second, func() {
				err := tree.DeleteNodes(deleteEventCache)
				if err != nil {
					log.Errorf("Failed to delete nodes: %v", err)
				} else {
					log.Debugf("Deleted nodes count %d", len(deleteEventCache))
					deleteEventCache = slices.Delete(deleteEventCache, 0, len(deleteEventCache))
				}
				deleteTimerActive = false
			})
			deleteTimerActive = true
		}
	}
}
