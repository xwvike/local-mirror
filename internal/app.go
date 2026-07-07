package app

import (
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
	if err := tree.BuildFileTree(config.StartPath); err != nil {
		log.Fatalf("构建文件树失败: %v", err)
	}

	switch *config.Mode {
	case "reality":
		_watcher, err := fsnotify.NewWatcher()
		if err != nil {
			log.Fatal(err)
		}
		defer func() {
			log.Info("正在关闭监视器...")
			if err := _watcher.Close(); err != nil {
				log.Errorf("关闭监视器时出错: %v", err)
			}
		}()
		if err := watcher.InitWatcher(_watcher); err != nil {
			log.Fatalf("初始化监视器失败: %v", err)
		}
		go Reality()
	case "mirror":
		go Mirror()
	default:
		log.Fatalf("未知运行模式: %s (可选: reality, mirror)", *config.Mode)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
}
