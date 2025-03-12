package app

import (
	"filetranslate/pkg/utils"
	"fmt"

	"github.com/fsnotify/fsnotify"
)

type Leaf struct {
	Name     string
	Path     string
	Type     string
	Children []Leaf
	Parent   *Leaf
	Metadata map[string]interface{}
}

var treeList []Leaf

func App() {
	osInfo := utils.BaseOSInfo()
	root := buildFileTree(osInfo.UserHomeDir + "/WebstormProjects")
	PrintFileTree(root, "|——")
	WatchFile(osInfo.UserHomeDir+"/TEST", func(event fsnotify.Event) {
		treeList = eventFilter(event, treeList)
		// fmt.Println(treeList)
	})
	fmt.Println(osInfo)
}
