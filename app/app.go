package app

import (
	"fmt"
	"local-mirror/app/tree"
	"local-mirror/config"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

func App() {
	pid := os.Getpid()
	fmt.Printf("进程 PID 啊: %d\n", pid)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()
	tree.BuildFileTree(config.StartPath)
	dirs, _ := tree.GetAllDirectories()
	watcherCount := 0
	for _, dir := range dirs {
		err = watcher.Add(dir.Path)
		if err != nil {
			fmt.Printf("watcher count %d,  error: %v", watcherCount, err)
			continue
		}

		watcherCount++
		// 使用 lsof 获取文件描述符使用情况
		cmd := exec.Command("sh", "-c", fmt.Sprintf("lsof -p %d | wc -l", pid))
		output, err := cmd.Output()
		if err != nil {
			fmt.Printf("获取文件描述符数量失败")
		} else {
			count := string(output)
			fmt.Printf("当前进程打开的文件描述符数量: %s", count)
		}
	}
	fmt.Printf("已添加 %d 个目录到文件监视器", watcherCount)
	// WatchFile(watcher)
	CreateLink()
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	watcher.Close()
}
