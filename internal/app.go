package app

import (
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/tree"
	"local-mirror/internal/watcher"
	"os"
	"os/signal"
	"syscall"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

func App() {
	pid := os.Getpid()
	fmt.Printf("进程 PID 啊: %d\n", pid)
	_watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if *config.Mode == "reality" {
			log.Info("正在关闭监视器...")
			if err := _watcher.Close(); err != nil {
				log.Errorf("关闭监视器时出错: %v", err)
			}
		}
	}()
	tree.BuildFileTree(config.StartPath)
	switch *config.Mode {
	case "reality":
		watcher.InitWatcher(_watcher)
		go Reality()
	case "mirror":
		go Mirror()
	}
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
}
