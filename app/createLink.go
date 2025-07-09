package app

import (
	"local-mirror/common/data"
	"local-mirror/common/jsonutil"
	"local-mirror/config"
	"net"
	"os"

	log "github.com/sirupsen/logrus"
)

var NextLevel = data.NewStack[jsonutil.DiffResult]()

func getDirectory(conn net.Conn, fileClient *fileClient, path string) {
	treejson, err := fileClient.GetRealityTree(conn, path)
	if err != nil {
		log.Fatal("Error getting reality tree:", err)
		os.Exit(1)
	}
	log.Info("start analyzing diff 🫨")
	Diff(treejson, path)
	log.Infof("Diff count: %d", diffQueue.Size())
	for diffQueue.Size() > 0 {
		v, has := diffQueue.Pop()
		if !has {
			log.Error("Diff queue is empty, but we expected more items")
			continue
		} else {
			log.Infof("Processing diff item: %v 【%d】remaining", v, diffQueue.Size())
			switch v.Action {
			case "delete":

			case "create", "modify":
				if v.IsDir {
					os.MkdirAll(v.Path, 0755)

					NextLevel.Push(v)
				} else {
					err := fileClient.DownloadFile(conn, v.Path)
					if err != nil {
						log.Errorf("File %s downloading fail, %v", v.Path, err)
					} else {
						log.Infof("File %s downloaded successfully", v.Path)
					}
				}
			}
		}
	}
}

func CreateLink() {
	switch *config.Mode {
	case "reality":
		log.Info("step 3 >> start file server")
		fileServer := NewFileServer("0.0.0.0:52345")
		if err := fileServer.Start(); err != nil {
			log.Fatal("Error starting file server:", err)
			os.Exit(1)
		}
	case "mirror":
		log.Info("step 3 >> start file client")
		fileClient := NewFileClient("10.10.0.5:52345")
		conn, err := fileClient.Connect()
		if err != nil {
			log.Fatal("Error connecting to file server:", err)
			os.Exit(1)
		}
		NextLevel.Push(jsonutil.DiffResult{
			Path:   ".",
			IsDir:  true,
			Action: "create",
			Name:   "root",
			Size:   0,
		})
		for NextLevel.Size() > 0 {
			v, has := NextLevel.Pop()
			if !has {
				log.Error("NextLevel stack is empty, but we expected more items")
				continue
			} else {
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
