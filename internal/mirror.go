package app

import (
	"errors"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/internal/network"
	"local-mirror/internal/tree"
	"local-mirror/pkg/stack"
	"local-mirror/pkg/utils"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
)

var NextLevel = stack.NewStack[DiffResult]()

func getDirectory(fileClient *network.FileClient, path string) error {
	treejson, err := fileClient.GetRealityTree(path)
	if err != nil {
		if errors.Is(err, appError.ErrConnection) {
			fileClient.ConnectionClose()
			return err
		} else {
			_error := fmt.Errorf("failed to get reality tree for path %s: %w", path, err)
			return _error
		}
	}
	log.Debug("start analyzing diff 🫨")
	err = Diff(treejson, path)
	if err != nil {
		_error := fmt.Errorf("error analyzing diff for path %s: %w", path, err)
		return _error
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
					hash, err := fileClient.DownloadFile(v.Path)
					if err != nil {
						if errors.Is(err, appError.ErrConnection) {
							fileClient.ConnectionClose()
							return err
						}
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
	return nil
}

func Mirror() {
	log.Debug("step 3 >> start file client")
	fileClient, err := InitConn()
	if err != nil {
		fileClient.ConnectionClose()
	}
	if fileClient.State == network.Offline {
		retryConnection := time.NewTicker(10 * time.Second)
		defer retryConnection.Stop()
		for range retryConnection.C {
			log.Info("Retrying connection to reality server...")
			fileClient, err = InitConn()
			if err == nil {
				log.Info("Successfully connected to reality server")
				retryConnection.Stop()
				break
			} else {
				log.Errorf("Failed to connect to reality server: %v", err)
			}
		}
	}
	fullScan(fileClient)

	fullScanTicker := time.NewTicker(time.Duration(*config.CoolDown) * time.Second)
	changeTicker := time.NewTicker(10 * time.Second)
	defer fullScanTicker.Stop()
	defer changeTicker.Stop()
	for {
		select {
		case <-fullScanTicker.C:
			fullScanTicker.Reset(24 * time.Hour)
			log.Info("Starting full scan...")
			err := fullScan(fileClient)
			fullScanTicker.Reset(time.Duration(*config.CoolDown) * time.Second)
			if err != nil {
				log.Errorf("Error during full scan: %v", err)
			}
		case <-changeTicker.C:
			changeTicker.Reset(24 * time.Hour)
			log.Info("Tracking changes...")
			err := TrackingChanges(fileClient)
			changeTicker.Reset(10 * time.Second)
			if err != nil {
				log.Errorf("Error tracking changes: %v", err)
			}
		}
	}

}

func fullScan(fileClient *network.FileClient) error {
	startTime := time.Now().UnixMilli()
	rootNode := DiffResult{
		Path:   ".",
		IsDir:  true,
		Action: "create",
		Name:   "root",
		Size:   0,
	}
	NextLevel.Push(rootNode)
	for NextLevel.Size() > 0 {
		v, _ := NextLevel.Pop()
		log.Infof("Processing next level item: %v 【%d】remaining", v, NextLevel.Size())
		if v.IsDir {
			err := getDirectory(fileClient, v.Path)
			if err != nil {
				log.Errorf("Error processing directory %s: %v", v.Path, err)
				if errors.Is(err, appError.ErrConnection) {
					err := fileClient.Reconnect()
					if err != nil {
						log.Errorf("Reconnection failed: %v", err)
						return err
					}
					fileClient.State = network.Online
					NextLevel.Push(v) // Re-push the directory to retry
				}
			}
		} else {
			log.Error("Unexpected file type in NextLevel stack, expected directory but got file:", v.Path)
		}

	}
	log.Info("Full scan completed, total time taken:", (time.Now().UnixMilli()-startTime)/1000, "seconds")
	return nil
}

func TrackingChanges(fileClient *network.FileClient) error {
	change, err := fileClient.GetTreeChange(100)
	if err != nil {
		if errors.Is(err, appError.ErrConnection) {
			fileClient.ConnectionClose()
			return err
		} else {
			return fmt.Errorf("failed to get tree change: %w", err)
		}
	}
	if len(change) == 0 {
		log.Info("No changes detected in the tree")
		return nil
	}
	fmt.Print(change)
	return nil
}
