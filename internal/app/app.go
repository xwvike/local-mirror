package app

import (
	"fmt"
	"local-mirror/config"
	"local-mirror/pkg/utils"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

type Leaf struct {
	Name         string                 `json:"name"`
	Path         string                 `json:"-"`
	RelativePath string                 `json:"path"` // Relative path from the start path
	Type         string                 `json:"type"` // "file" or "dir"
	Children     []*Leaf                `json:"children"`
	Parent       *Leaf                  `json:"-"`
	Metadata     map[string]interface{} `json:"metadata,omitempty"` // Additional metadata like size, mode, modTime, etc.
	mu           sync.Mutex             `json:"-"`
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

var (
	rootLeaf       *Leaf
	ignoreFileList = []string{".git", ".gitingore", ".github", ".local-mirror", ".DS_Store"}
)

func App() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()
	osInfo := utils.BaseOSInfo()
	rootLeaf = buildFileTree(config.StartPath)
	WatchFile(watcher)
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
		treejson, err := fileClient.GetRealityTree(conn, ".")
		if err != nil {
			log.Fatal("Error getting reality tree:", err)
			os.Exit(1)
		}
		Diff(treejson, rootLeaf)
		for _, v := range diffQueue {
			if v.Type == "file" && v.Action == "add" {
				err := fileClient.DownloadFile(conn, v.Path)
				if err != nil {
					log.Errorf("Error downloading file %s: %v", v.Path, err)
				} else {
					log.Infof("File %s downloaded successfully", v.Path)
				}
			}
		}
		defer conn.Close()
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	watcher.Close()
}
