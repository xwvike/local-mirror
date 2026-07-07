package app

import (
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/network"

	log "github.com/sirupsen/logrus"
)

func Reality() {
	log.Debug("step 3 >> start file server")
	fileServer := network.NewFileServer(fmt.Sprintf("0.0.0.0:%d", config.DefaultPort))
	// Start 阻塞在 accept 循环中，只有监听失败或严重错误才会返回
	if err := fileServer.Start(); err != nil {
		log.Fatal("Error starting file server:", err)
	}
}
