package app

import (
	"filetranslate/pkg/utils"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/fsnotify/fsnotify"
)

type Leaf struct {
	Name     string
	Path     string
	Type     string
	Children []*Leaf
	Parent   *Leaf
	Metadata map[string]interface{}
	mu       sync.Mutex
}

func (l *Leaf) Format2JSON() map[string]interface{} {
	l.mu.Lock()
	defer l.mu.Unlock()

	leaf := map[string]interface{}{
		"name":     l.Name,
		"path":     l.Path,
		"type":     l.Type,
		"children": []map[string]interface{}{},
	}

	for _, child := range l.Children {
		childJSON := child.Format2JSON()
		leaf["children"] = append(leaf["children"].([]map[string]interface{}), childJSON)
	}

	return leaf
}

func (l *Leaf) AddChild(child *Leaf) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.Children = append(l.Children, child)
}
func (l *Leaf) RemoveChild(child *Leaf) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for i, c := range l.Children {
		if c.Path == child.Path {
			l.Children = append(l.Children[:i], l.Children[i+1:]...)
			break
		}
	}
}
func (l *Leaf) GetAllDirs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var dirs []string
	if l.Type == "dir" {
		dirs = append(dirs, l.Path)
		for _, child := range l.Children {
			dirs = append(dirs, child.GetAllDirs()...)
		}
	}
	return dirs
}

func App() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()
	osInfo := utils.BaseOSInfo()
	root := buildFileTree(osInfo.UserHomeDir + "/TEST")
	WatchFile(watcher, root)
	fmt.Println(osInfo)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	watcher.Close()
}
