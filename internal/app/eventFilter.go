package app

import (
	"filetranslate/pkg/utils"
	"strings"

	"github.com/fsnotify/fsnotify"
)

var ignoreFileList = []string{".DS_Store"}
var (
	create string = "CREATE"
	remove string = "REMOVE"
)

func eventFilter(event fsnotify.Event, treeList []Leaf) []Leaf {
	opStr := event.Op.String()
	filepath := event.Name
	hasIgnore := false
	os := utils.BaseOSInfo().OS
	for _, v := range ignoreFileList {
		if strings.Contains(filepath, v) {
			hasIgnore = true
			break
		}
	}
	if hasIgnore {
		return treeList
	}

	if os == "darwin" {
		create = "CREATE"
		remove = "RENAME"
	} else if os == "linux" {

	} else if os == "windows" {

	} else {
		return treeList
	}
	if opStr == create {
		has := false
		for _, v := range treeList {
			if v.Path == filepath {
				has = true
				break
			}
		}
		if !has {
			treeList = append(treeList, getLeafInfo(filepath))
		}
	} else if opStr == remove {
		for i, v := range treeList {
			if v.Path == filepath {
				treeList = append(treeList[:i], treeList[i+1:]...)
				break
			}
		}
	}
	return treeList
}
