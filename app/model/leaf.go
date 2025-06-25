package model

import ()

type Leaf struct {
	Name         string  `json:"name"`
	Path         string  `json:"-"`
	RelativePath string  `json:"path"` // Relative path from the start path
	Type         uint8   `json:"type"` // 0: file, 1: dir
	Children     []*Leaf `json:"children"`
	Deep         int     `json:"deep"` // Depth in the tree
	Size         uint64  `json:"size"` // Size in bytes
}

func (l *Leaf) AddChild(child *Leaf) {
	l.Children = append(l.Children, child)
}
func (l *Leaf) RemoveChild(child *Leaf) {
	for i, c := range l.Children {
		if c.Path == child.Path {
			l.Children = append(l.Children[:i], l.Children[i+1:]...)
			break
		}
	}
}
func (l *Leaf) GetChild(path string) *Leaf {

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

	var files []string
	switch l.Type {
	case 0:
		files = append(files, l.Path)
	case 1:
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
	RootLeaf       *Leaf
	IgnoreFileList = []string{"Library", ".gitingore", ".git", "node_modules", ".github", ".local-mirror", ".DS_Store", "server.log", "largeFile.log"}
)
