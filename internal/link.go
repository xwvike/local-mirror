package app

import (
	log "github.com/sirupsen/logrus"
	"local-mirror/config"
	"local-mirror/internal/network"
	"local-mirror/internal/tree"
	"local-mirror/pkg/stack"
	"local-mirror/pkg/utils"
	"net"
	"os"
	"path/filepath"
	"time"
)

var NextLevel = stack.NewStack[DiffResult]()

func getDirectory(conn net.Conn, fileClient *network.FileClient, path string) {
	treejson, err := fileClient.GetRealityTree(conn, path)
	if err != nil {
		log.Errorf("get reality tree for path %s: %v", path, err)
		return
	}
	log.Debug("start analyzing diff 🫨")
	err = Diff(treejson, path)
	if err != nil {
		log.Errorf("Diff error: %v", err)
		return
	}
	log.Infof("Diff count: %d", diffQueue.Size())
	for diffQueue.Size() > 0 {
		v, has := diffQueue.Pop()
		if !has {
			log.Warn("Diff queue is empty")
			continue
		} else {
			log.Debugf("Processing diff item: %v 【%d】remaining", v, diffQueue.Size())
			switch v.Action {
			case "delete":
				err := os.RemoveAll(filepath.Join(config.StartPath, v.Path))
				if err != nil {
					tree.DeleteNode(v.Path)
				}
			case "create", "modify":
				if v.IsDir {
					os.MkdirAll(v.Path, 0755)
					hasPaht, err := tree.HasPath(v.Path)
					if err == nil {
						if !hasPaht {
							uuid, _ := utils.RandomString(16)
							node := &tree.Node{
								ID:       uuid,
								Path:     v.Path,
								Name:     v.Name,
								ParentID: v.ParentID,
								IsDir:    v.IsDir,
								Size:     v.Size,
								ModTime:  time.Now(),
								Hash:     "",
							}

							tree.AddNodes([]*tree.Node{node})
						}
						NextLevel.Push(v)
					} else {
						log.Fatalf("Error checking path %s: %v", v.Path, err)
					}
				} else {
					hash, err := fileClient.DownloadFile(conn, v.Path)
					if err != nil {
						log.Errorf("Error downloading file %s: %v", v.Path, err)
					} else {
						uuid, _ := utils.RandomString(16)
						fileNode := &tree.Node{
							ID:       uuid,
							Path:     v.Path,
							Name:     v.Name,
							ParentID: v.ParentID,
							IsDir:    v.IsDir,
							Size:     v.Size,
							ModTime:  time.Now(),
							Hash:     hash,
						}
						tree.AddNodes([]*tree.Node{fileNode})
						log.Infof("File downloaded successfully: %s", v.Path)
					}
				}
			}
		}
	}
}

func CreateLink() {
	switch *config.Mode {
	case "reality":
		log.Debug("step 3 >> start file server")
		fileServer := network.NewFileServer("0.0.0.0:52345")
		if err := fileServer.Start(); err != nil {
			log.Fatal("Error starting file server:", err)
			os.Exit(1)
		}
	case "mirror":
		log.Debug("step 3 >> start file client")
		fileClient := network.NewFileClient(*config.RealityIP + ":52345")
		conn, err := fileClient.Connect()
		if err != nil {
			log.Fatal("connecting to file server fail:", err)
		}
		rootNode := DiffResult{
			Path:   ".",
			IsDir:  true,
			Action: "create",
			Name:   "root",
			Size:   0,
		}
		NextLevel.Push(rootNode)
		var coolTime = time.Now().UnixMilli()
		for NextLevel.Size() > 0 {
			v, has := NextLevel.Pop()
			if !has {
				elapsed := time.Now().UnixMilli() - coolTime
				if elapsed > *config.CoolDown*1000 {
					NextLevel.Push(rootNode)
					coolTime = time.Now().UnixMilli()
					log.Infof("Cool down period elapsed, restarting from root")
				} else {
					remaining := *config.CoolDown*1000 - elapsed
					sleepTime := min(1000, remaining/10)
					time.Sleep(time.Duration(sleepTime) * time.Millisecond)
				}
				continue
			} else {
				coolTime = time.Now().UnixMilli()
				log.Infof("Processing next level item: %v 【%d】remaining", v, NextLevel.Size())
				if v.IsDir {
					getDirectory(conn, fileClient, v.Path)
				} else {
					log.Error("Unexpected file type in NextLevel stack, expected directory but got file:", v.Path)
				}
			}
		}

	}
}
