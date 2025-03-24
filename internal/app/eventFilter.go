package app

import (
	"filetranslate/pkg/utils"
	"fmt"
	"strings"

	"github.com/fsnotify/fsnotify"
)

var (
	create string = "CREATE"
	remove string = "REMOVE"
)

func eventFilter(event fsnotify.Event, node *Leaf) {
	fmt.Println(node.Path, "create", event)
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

	}

	if os == "darwin" {
		create = "CREATE"
		remove = "RENAME"
	} else if os == "linux" {

	} else if os == "windows" {

	} else {

	}
	if opStr == create {

		// has := false
		// for _, v := range treeList {
		// 	if v.Path == filepath {
		// 		has = true
		// 		break
		// 	}
		// }
		// if !has {
		// 	treeList = append(treeList, getLeafInfo(filepath))
		// }
	} else if opStr == remove {
		// for i, v := range treeList {
		// 	if v.Path == filepath {
		// 		treeList = append(treeList[:i], treeList[i+1:]...)
		// 		break
		// 	}
		// }
	}
}
