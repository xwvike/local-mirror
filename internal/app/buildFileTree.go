package app

import (
	"local-mirror/config"
	"os"
	"path/filepath"
	"strings"
)

func getLeafInfo(filepath string) Leaf {
	fileInfo, err := os.Stat(filepath)
	var ignore = false
	for _, v := range ignoreFileList {
		if strings.Contains(filepath, v) {
			ignore = true
			break
		}
	}
	if ignore {
		return Leaf{}
	}
	if err != nil {
		return Leaf{}
	}
	fileType := "file"
	if fileInfo.IsDir() {
		fileType = "dir"
	}
	return Leaf{
		Name:         fileInfo.Name(),
		Path:         filepath,
		RelativePath: strings.Replace(filepath, config.StartPath, ".", 1),
		Type:         fileType,
		Metadata: map[string]interface{}{
			"size":    fileInfo.Size(),
			"modTime": fileInfo.ModTime(),
		},
		Children: []*Leaf{},
		Parent:   nil,
	}
}

func buildFileTree(path string) *Leaf {
	rootNode := getLeafInfo(path)

	if rootNode.Type == "dir" {
		buildChildren(&rootNode, path)
	}

	return &rootNode
}

func buildChildren(parent *Leaf, dirPath string) {
	entries, err := os.ReadDir(dirPath)

	if err != nil {
		return
	}

	for _, entry := range entries {
		childPath := filepath.Join(dirPath, entry.Name())
		childNode := getLeafInfo(childPath)

		childNode.Parent = parent
		parent.Children = append(parent.Children, &childNode)

		if childNode.Type == "dir" {
			childPtr := parent.Children[len(parent.Children)-1]
			buildChildren(childPtr, childPath)
		}
	}
}
