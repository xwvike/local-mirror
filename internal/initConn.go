package app

import (
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/network"

	log "github.com/sirupsen/logrus"
)

// InitConn 在 [DefaultPort, DefaultPort+PortScanRange) 范围内逐个端口探测服务端。
// 服务端启动时会自动选择该范围内第一个可用端口，客户端因此不能只连固定端口；
// 用握手确认对端确实是 local-mirror 服务端，避免误连到恰好占用端口的其他程序。
// 单轮探测失败直接返回错误，重试交给 Mirror 主循环的退避逻辑。
func InitConn() (*network.FileClient, error) {
	ip := *config.RealityIP
	if ip == "" {
		// 自动发现尚未实现，回退到本机地址
		log.Warn("未指定服务器地址 (-r)，回退连接 127.0.0.1")
		ip = "127.0.0.1"
	}

	var lastErr error
	for port := config.DefaultPort; port < config.DefaultPort+config.PortScanRange; port++ {
		addr := fmt.Sprintf("%s:%d", ip, port)
		log.Debugf("探测服务端端口: %s", addr)

		fileClient, err := network.NewFileClient(addr, "default")
		if err != nil {
			// 端口未开放，尝试下一个
			lastErr = err
			continue
		}
		if err := fileClient.Handshake(); err != nil {
			log.Warnf("端口 %d 握手失败（可能被其他程序占用）: %v", port, err)
			fileClient.ConnectionClose()
			lastErr = err
			continue
		}
		log.Infof("已连接服务端 %s", addr)
		return fileClient, nil
	}

	// 返回非 nil 的占位 client，调用方（ensureConnected）依赖非 nil 返回值
	dummy := &network.FileClient{RealityAddr: ip, Alias: "default", State: network.Offline}
	return dummy, fmt.Errorf("在 %s 的端口 %d-%d 范围内未找到 local-mirror 服务端: %w",
		ip, config.DefaultPort, config.DefaultPort+config.PortScanRange-1, lastErr)
}
