package app

import (
	"log"
	"strings"

	"github.com/fsnotify/fsnotify"
)

var ignoreFileList = []string{".mirror", ".DS_Store"}

func WatchFile(watcher *fsnotify.Watcher, root *Leaf) {
	for _, dir := range root.GetAllDirs() {
		for _, v := range ignoreFileList {
			if strings.Contains(dir, v) {
				continue
			}
		}
		err := watcher.Add(dir)
		if err != nil {
			log.Fatal(err)
		}
	}
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				eventFilter(event, watcher, root)
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("error:", err)
			}
		}
	}()
}
