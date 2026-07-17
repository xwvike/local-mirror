//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"

	"local-mirror/internal/watcher"
)

// installHeatDumpSignal 注册 SIGUSR1：随时给进程发信号即可把目录热度
// 快照写到 <同步根>/.local-mirror/heat.txt，不打扰正常运行。
// 用法：kill -USR1 <pid> && cat <同步根>/.local-mirror/heat.txt
func installHeatDumpSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		for range ch {
			watcher.WriteHeatSnapshot()
		}
	}()
}
