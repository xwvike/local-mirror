package app

import (
	"fmt"
	"local-mirror/config"
	"local-mirror/pkg/utils"
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
func (l *Leaf) GetChild(path string) *Leaf {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Check if the current leaf matches the path
	if l.Path == path {
		return l
	}

	// Recursively search in children
	for _, child := range l.Children {
		if result := child.GetChild(path); result != nil {
			return result
		}
	}
	return nil
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
	root := buildFileTree(osInfo.UserHomeDir + "/test")
	WatchFile(watcher, root)
	fmt.Println(osInfo)
	if *config.Mode == "reality" {
		fileServer := NewFileServer("0.0.0.0:52345")
		if err := fileServer.Start(); err != nil {
			log.Fatal("Error starting file server:", err)
			os.Exit(1)
		}
	} else if *config.Mode == "mirror" {
		fileClient := NewFileClient("10.8.0.9:52345")
		conn, err := fileClient.Connect()
		if err != nil {
			log.Fatal("Error connecting to file server:", err)
			os.Exit(1)
		}
		fileClient.DownloadFile(conn, "./ds.mp4")
		defer conn.Close()
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	watcher.Close()
}
