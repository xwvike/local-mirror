package app

import (
	"fmt"
	"log"
	"strings"

	"github.com/fsnotify/fsnotify"
)

var ignoreFileList = []string{".mirror", ".DS_Store"}

func WatchFile(watcher *fsnotify.Watcher, node *Leaf) {
	for _, v := range ignoreFileList {
		if strings.Contains(node.Path, v) {
			return
		}
	}
	if node.Type == "dir" {
		err := watcher.Add(node.Path)
		if err != nil {
			log.Fatal(err)
		}

		go func() {
			fmt.Println(node.Path, node.Type)
			for {
				select {
				case event, ok := <-watcher.Events:
					if !ok {
						return
					}
					eventFilter(event, node)
				case err, ok := <-watcher.Errors:
					if !ok {
						return
					}
					log.Println("error:", err)
				}
			}
		}()
		for _, child := range node.Children {
			WatchFile(watcher, child)
		}
	} else {
		return
	}
}
