package app

import (
	"local-mirror/config"
	"local-mirror/internal/network"
	"net"
	"strconv"

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

// RealityDial 源拨出格（--send --connect，四象限）：不监听，主动拨向
// 监听中的汇并在拨出的连接上服务。端口缺省用 DefaultPort——公网部署
// 钉死单端口，不做端口段扫描（那是局域网发现时代的特性）
func RealityDial() {
	log.Debug("step 3 >> start file server (dial-out)")
	host, port := network.SplitPeer(*config.RealityIP)
	if port == 0 {
		port = config.DefaultPort
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	// StartDial 阻塞：拨号、服务、断开重拨（退避归拨号方）
	network.NewFileServerDial().StartDial(addr)
}
