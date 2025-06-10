package app

import (
	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
	"local-mirror/common/utils"
	"local-mirror/config"
	"path/filepath"
	"strings"
)

var (
	create string = "CREATE"
	remove string = "REMOVE"
)

func eventFilter(event fsnotify.Event, watcher *fsnotify.Watcher) {
	ignored := false
	for _, v := range ignoreFileList {
		if strings.Contains(event.Name, v) {
			ignored = true
			break
		}
	}
	if ignored {
		return
	}
	nodeDir := filepath.Dir(event.Name)
	fatherNode := rootLeaf.GetChild(nodeDir)
	if fatherNode == nil {
		return
	}
	opStr := event.Op.String()
	os := utils.BaseOSInfo().OS
	if os == "darwin" {
		create = "CREATE"
		remove = "REMOVE"
	} else if os == "linux" {

	} else if os == "windows" {

	} else {

	}
	if opStr == create {
		isDir, err := utils.IsDir(event.Name)
		if err != nil {
			log.Error("Error checking if path is directory:", err)
			return
		}
		fileType := "file"
		if isDir {
			fileType = "dir"
		}
		newLeaf := &Leaf{
			Name:         filepath.Base(event.Name),
			Path:         event.Name,
			RelativePath: strings.Replace(event.Name, config.StartPath, ".", 1),
			Type:         fileType,
			Children:     []*Leaf{},
			Parent:       fatherNode,
			Metadata:     map[string]interface{}{},
		}
		size, err := utils.GetSize(event.Name)
		if err == nil {
			newLeaf.Metadata["size"] = size
		}
		modTime, err := utils.GetModTime(event.Name)
		if err == nil {
			newLeaf.Metadata["modTime"] = modTime
		}
		mode, err := utils.GetMode(event.Name)
		if err == nil {
			newLeaf.Metadata["mode"] = mode
		}
		fatherNode.AddChild(newLeaf)
		if fileType == "dir" {
			err := watcher.Add(event.Name)
			if err != nil {
				log.Error("Error adding directory to watcher:", err)
			}
		}
	} else if opStr == remove {
		children := fatherNode.GetChild(event.Name)
		if children == nil {
			return
		}
		fatherNode.RemoveChild(children)
	}
}
