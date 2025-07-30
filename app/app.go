package app

import (
	"fmt"
	"local-mirror/app/tree"
	"local-mirror/app/watcher"
	"local-mirror/config"
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
	defer _watcher.Close()
	tree.BuildFileTree(config.StartPath)
	watcher.InitWatcher(_watcher)
	CreateLink()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	_watcher.Close()
}
