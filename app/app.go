package app

import (
	"local-mirror/config"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

type Leaf struct {
	Name         string     `json:"name"`
	Path         string     `json:"-"`
	RelativePath string     `json:"path"` // Relative path from the start path
	Type         uint8      `json:"type"` // 0: file, 1: dir
	Children     []*Leaf    `json:"children"`
	Deep         int        `json:"deep"` // Depth in the tree
	Size         uint64     `json:"size"` // Size in bytes
	mu           sync.Mutex `json:"-"`
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

	if l.Path == path {
		return l
	}

	for _, child := range l.Children {
		if result := child.GetChild(path); result != nil {
			return result
		}
	}
	return nil
}
func (l *Leaf) GetAllDirs(deep uint16) []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var dirs []string
	if l.Type == 1 {
		dirs = append(dirs, l.Path)
		if deep > 0 {
			for _, child := range l.Children {
				if child.Type == 1 {
					dirs = append(dirs, child.Path)
					if deep > 1 {
						childDirs := child.GetAllDirs(deep - 1)
						dirs = append(dirs, childDirs...)
					}
				}
			}
		}
	}
	return dirs
}

func (l *Leaf) GetAllFiles(deep uint16) []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var files []string
	if l.Type == 0 {
		files = append(files, l.Path)
	} else if l.Type == 1 {
		for _, child := range l.Children {
			if child.Type == 0 {
				files = append(files, child.Path)
			} else if deep > 0 && child.Type == 1 {
				childFiles := child.GetAllFiles(deep - 1)
				files = append(files, childFiles...)
			}
		}
	}
	return files
}

var (
	rootLeaf       *Leaf
	ignoreFileList = []string{".gitingore", ".git", "node_modules", ".github", ".local-mirror", ".DS_Store", "server.log", "largeFile.log"}
)

func App() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()
	rootLeaf = buildFileTreeTwoPhase(config.StartPath)
	WatchFile(watcher)
	CreateLink()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	watcher.Close()
}
