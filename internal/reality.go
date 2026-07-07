package app

import (
	"local-mirror/internal/network"

	log "github.com/sirupsen/logrus"
)

func Reality() {
	log.Debug("step 3 >> start file server")
	fileServer := network.NewFileServer("0.0.0.0:52345")
	// Start 阻塞在 accept 循环中，只有监听失败或严重错误才会返回
	if err := fileServer.Start(); err != nil {
		log.Fatal("Error starting file server:", err)
	}
}
