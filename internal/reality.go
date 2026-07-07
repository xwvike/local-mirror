package app

import (
	"local-mirror/internal/network"
	"net"

	log "github.com/sirupsen/logrus"
)

// ServerListener 由 main 在打印启动横幅前绑定好（端口自动探测），
// 这里只负责在其上启动服务
var ServerListener net.Listener

func Reality() {
	log.Debug("step 3 >> start file server")
	if ServerListener == nil {
		log.Fatal("server listener not initialized")
	}
	fileServer := network.NewFileServer(ServerListener)
	// Start 阻塞在 accept 循环中，只有监听器关闭或严重错误才会返回
	if err := fileServer.Start(); err != nil {
		log.Fatal("Error starting file server:", err)
	}
}
