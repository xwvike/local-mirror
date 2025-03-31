package app

import (
	"filetranslate/pkg/utils"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

var (
	create string = "CREATE"
	remove string = "REMOVE"
)

func eventFilter(event fsnotify.Event, watcher *fsnotify.Watcher, root *Leaf) {
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
	fatherNode := root.GetChild(nodeDir)
	if fatherNode == nil {
		return
	}
	opStr := event.Op.String()
	os := utils.BaseOSInfo().OS
	if os == "darwin" {
		create = "CREATE"
		remove = "RENAME"
	} else if os == "linux" {

	} else if os == "windows" {

	} else {

	}
	if opStr == create {
		isDir, err := utils.IsDir(event.Name)
		if err != nil {
			fmt.Printf("Error checking if path is directory: %v\n", err)
			return
		}
		fileType := "file"
		if isDir {
			fileType = "dir"
		}
		newLeaf := &Leaf{
			Name:     filepath.Base(event.Name),
			Path:     event.Name,
			Type:     fileType,
			Children: []*Leaf{},
			Parent:   fatherNode,
			Metadata: map[string]interface{}{},
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
				fmt.Println("Error adding directory to watcher:", err)
			}
			fmt.Println("Added directory to watcher:", event.Name)
		} else {
			fmt.Println("Added file", event.Name)
		}
	} else if opStr == remove {
		// for i, v := range treeList {
		// 	if v.Path == filepath {
		// 		treeList = append(treeList[:i], treeList[i+1:]...)
		// 		break
		// 	}
		// }
	}
}
