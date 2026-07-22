package app

import (
	"errors"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/network"
	"net"
	"strconv"

	log "github.com/sirupsen/logrus"
)

// InitConn 在 [DefaultPort, DefaultPort+PortScanRange) 范围内逐个端口探测服务端。
// 服务端启动时会自动选择该范围内第一个可用端口，客户端因此不能只连固定端口；
// 用握手确认对端确实是 local-mirror 服务端，避免误连到恰好占用端口的其他程序。
// 单轮探测失败直接返回错误，重试交给 Mirror 主循环的退避逻辑。
func InitConn() (*network.FileClient, error) {
	// -r/--connect 收 host[:port]：IPv4 / IPv6 字面量 / 域名，端口可选。
	// 域名交给 Dial 每次重新解析（DDNS 友好，不缓存 IP——见
	// docs/PUBLIC_EXPOSURE.md §B.3）
	ip, exactPort := network.SplitPeer(*config.RealityIP)
	exactAddr := ""
	if ip == "" {
		if config.DiscoveredAddr != "" {
			// 自动发现选定的精确地址优先直连；端口段扫描保留为后备
			// （服务端重启可能落到相邻端口，重连时靠扫描自愈）
			exactAddr = config.DiscoveredAddr
			if host, _, err := net.SplitHostPort(exactAddr); err == nil {
				ip = host
			}
		} else {
			// 防御性回退：main 的发现流程已保证正常路径不会走到这里
			log.Warn("no server address (-r) given, falling back to 127.0.0.1")
			ip = "127.0.0.1"
		}
	}

	candidates := make([]string, 0, config.PortScanRange+1)
	if exactAddr != "" {
		candidates = append(candidates, exactAddr)
	}
	if exactPort != 0 {
		// 显式给了端口（--connect host:port）：钉死单端口，不做端口段扫描
		//（公网部署常态；扫描是局域网发现时代的特性）
		candidates = append(candidates, net.JoinHostPort(ip, strconv.Itoa(exactPort)))
	} else {
		for port := config.DefaultPort; port < config.DefaultPort+config.PortScanRange; port++ {
			// JoinHostPort 而非 Sprintf：v6 字面量需要方括号（[::1]:52345）
			addr := net.JoinHostPort(ip, strconv.Itoa(port))
			if addr != exactAddr {
				candidates = append(candidates, addr)
			}
		}
	}

	var lastErr error
	var handshakeErr error
	for _, addr := range candidates {
		log.Debugf("probing server port: %s", addr)

		fileClient, err := network.NewFileClient(addr, "default")
		if err != nil {
			if errors.Is(err, network.ErrSecureHandshake) {
				log.Warnf("%s: secure handshake failed: %v", addr, err)
				handshakeErr = err
			} else {
				// 端口未开放，尝试下一个
				lastErr = err
			}
			continue
		}
		if err := fileClient.Handshake(); err != nil {
			log.Warnf("%s: handshake failed (passphrase mismatch or port taken by another program): %v", addr, err)
			fileClient.ConnectionClose()
			// 端口开放但握手失败，比"端口拒连"更有定位价值，优先保留
			handshakeErr = err
			continue
		}
		log.Infof("connected to server %s", addr)
		return fileClient, nil
	}

	if handshakeErr != nil {
		lastErr = handshakeErr
	}
	// 返回非 nil 的占位 client，调用方（ensureConnected）依赖非 nil 返回值
	dummy := &network.FileClient{RealityAddr: ip, Alias: "default", State: network.Offline}
	return dummy, fmt.Errorf("no local-mirror server found on %s in port range %d-%d: %w",
		ip, config.DefaultPort, config.DefaultPort+config.PortScanRange-1, lastErr)
}
