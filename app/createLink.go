package app

import (
	log "github.com/sirupsen/logrus"
	"local-mirror/config"
	"net"
	"os"
	"time"
)

func mirror(conn net.Conn, fileClient *fileClient) {
	treejson, err := fileClient.GetRealityTree(conn, ".")
	if err != nil {
		log.Fatal("Error getting reality tree:", err)
		os.Exit(1)
	}
	log.Info("start analyzing diff 🫨")
	Diff(treejson, ".")
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
	time.Sleep(5 * time.Second)
	mirror(conn, fileClient)
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
		mirror(conn, fileClient)
	}
}
