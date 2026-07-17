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
		log.Fatalf("failed to build file tree: %v", err)
	}

	switch *config.Mode {
	case "reality":
		_watcher, err := fsnotify.NewWatcher()
		if err != nil {
			log.Fatal(err)
		}
		defer func() {
			log.Info("shutting down watcher...")
			if err := _watcher.Close(); err != nil {
				log.Errorf("error closing watcher: %v", err)
			}
		}()
		if err := watcher.InitWatcher(_watcher); err != nil {
			log.Fatalf("failed to init watcher: %v", err)
		}
		go Reality()
	case "mirror":
		go Mirror()
	case "relay":
		// 中继 = mirror 引擎 + reality 服务端，共享同一目录与数据库。
		// 不启动 fsnotify 监视器：中继目录的变更全部来自 mirror 引擎，
		// 它在应用每个 diff 后直接记录变更目录（见 recordChangedDir），
		// 比 watcher 更精确，且不受 tier2 冷目录轮询延迟影响
		go Reality()
		go Mirror()
	default:
		log.Fatalf("unknown mode: %s (valid: reality, mirror, relay)", *config.Mode)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
}
