package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/client"
	"local-mirror/internal/diff"
	"local-mirror/internal/server"
	"local-mirror/internal/tree"
	"local-mirror/internal/watcher"
	"local-mirror/pkg/data"
	"local-mirror/pkg/logger"
	"local-mirror/pkg/utils"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

func init() {
	config.InstanceID = utils.GenerateRandomUint32()
	config.StartTime = time.Now().Unix()
	wd, err := os.Getwd()
	if err != nil {
		log.Fatalf("获取当前执行文件路径失败: %v", err)
		os.Exit(1)
	}
	fmt.Print(wd)
	config.StartPath = wd
}

func main() {
	// 启动 pprof HTTP 服务
	go func() {
		if err := http.ListenAndServe("localhost:6060", nil); err != nil {
			log.Fatalf("pprof HTTP 服务启动失败: %v", err)
		}
	}()
	defer tree.DB.Close()
	flag.Parse()
	logger.Initialize()
	log.Infof("实例ID: %x", config.InstanceID)
	log.Infof("协议版本: %x", config.Version)
	log.Infof("运行模式: %s", *config.Mode)
	log.Infof("日志级别: %s", *config.LogLevel)
	log.Infof("启动时间: %d", config.StartTime)
	log.Infof("当前工作目录: %s", config.StartPath)
	tree.InitializeDatabase()

	app()
}

func app() {
	pid := os.Getpid()
	fmt.Printf("进程 PID: %d\n", pid)

	// 创建文件监视器
	fileWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer fileWatcher.Close()

	// 构建文件树
	tree.BuildFileTree(config.StartPath)

	// 初始化文件监视器
	watcher.Initialize(fileWatcher)

	// 根据模式启动相应的服务
	switch *config.Mode {
	case "reality":
		startRealityMode()
	case "mirror":
		startMirrorMode()
	default:
		log.Fatalf("Unknown mode: %s", *config.Mode)
	}

	// 等待信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fileWatcher.Close()
}

func startRealityMode() {
	log.Info("Starting in reality mode - file server")
	fileServer := server.NewFileServer("0.0.0.0:52345")
	go func() {
		if err := fileServer.Start(); err != nil {
			log.Fatal("Error starting file server:", err)
		}
	}()
}

func startMirrorMode() {
	log.Info("Starting in mirror mode - file client")

	if *config.RealityIP == "" {
		log.Fatal("Reality IP must be specified in mirror mode")
	}

	fileClient := client.NewFileClient(*config.RealityIP + ":52345")
	conn, err := fileClient.Connect()
	if err != nil {
		log.Fatal("Failed to connect to file server:", err)
	}
	defer conn.Close()

	// 创建同步队列
	nextLevel := data.NewStack[diff.Result]()

	// 初始根节点
	rootNode := diff.Result{
		Path:   ".",
		IsDir:  true,
		Action: "create",
		Name:   "root",
		Size:   0,
	}
	nextLevel.Push(rootNode)

	var coolTime = time.Now().UnixMilli()

	for !nextLevel.IsEmpty() {
		item, hasItem := nextLevel.Pop()
		if !hasItem {
			// 没有任务，检查冷却时间
			elapsed := time.Now().UnixMilli() - coolTime
			if elapsed > *config.CoolDown*1000 {
				nextLevel.Push(rootNode)
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
			log.Infof("Processing next level item: %v [%d remaining]", item, nextLevel.Size())
			if item.IsDir {
				processMirrorDirectory(conn, fileClient, item.Path, nextLevel)
			} else {
				log.Error("Unexpected file type in NextLevel stack, expected directory but got file:", item.Path)
			}
		}
	}
}

func processMirrorDirectory(conn net.Conn, fileClient *client.FileClient, path string, nextLevel *data.Stack[diff.Result]) {
	// 获取远程目录树
	treejson, err := fileClient.GetRealityTree(conn, path)
	if err != nil {
		log.Errorf("Failed to get reality tree for path %s: %v", path, err)
		return
	}

	log.Debug("Start analyzing diff 🫨")

	// 获取本地目录内容
	localTree, err := tree.GetDirectoryContents(path)
	if err != nil {
		log.Errorf("Failed to get local tree contents: %v", err)
		return
	}

	// 解析远程树数据
	var realityTreeData []tree.Node
	if err := json.Unmarshal(treejson, &realityTreeData); err != nil {
		log.Errorf("Failed to unmarshal reality tree: %v", err)
		return
	}

	// 比较并获取差异
	diffs := diff.CompareTreeStructures(realityTreeData, localTree)
	log.Infof("Diff count: %d", len(diffs))

	// 处理差异
	for _, diffItem := range diffs {
		log.Debugf("Processing diff item: %v", diffItem)
		switch diffItem.Action {
		case "delete":
			err := os.RemoveAll(filepath.Join(config.StartPath, diffItem.Path))
			if err != nil {
				tree.DeleteNode(diffItem.Path)
			}
		case "create", "modify":
			if diffItem.IsDir {
				os.MkdirAll(diffItem.Path, 0755)
				hasPath, err := tree.HasPath(diffItem.Path)
				if err == nil {
					if !hasPath {
						uuid, _ := utils.GenerateRandomString(16)
						node := &tree.Node{
							ID:       uuid,
							Path:     diffItem.Path,
							Name:     diffItem.Name,
							ParentID: diffItem.ParentID,
							IsDir:    diffItem.IsDir,
							Size:     diffItem.Size,
							ModTime:  time.Now(),
							Hash:     "",
						}
						tree.AddNodes([]*tree.Node{node})
					}
					nextLevel.Push(diffItem)
				} else {
					log.Fatalf("Error checking path %s: %v", diffItem.Path, err)
				}
			} else {
				hash, err := fileClient.DownloadFile(conn, diffItem.Path)
				if err != nil {
					log.Errorf("Error downloading file %s: %v", diffItem.Path, err)
				} else {
					uuid, _ := utils.GenerateRandomString(16)
					fileNode := &tree.Node{
						ID:       uuid,
						Path:     diffItem.Path,
						Name:     diffItem.Name,
						ParentID: diffItem.ParentID,
						IsDir:    diffItem.IsDir,
						Size:     diffItem.Size,
						ModTime:  time.Now(),
						Hash:     hash,
					}
					tree.AddNodes([]*tree.Node{fileNode})
					log.Infof("File downloaded successfully: %s", diffItem.Path)
				}
			}
		}
	}
}
