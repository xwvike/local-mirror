package network

import (
	"fmt"
	"net"
	"sync"

	log "github.com/sirupsen/logrus"
)

// ListenedDualStack 服务端实际监听栈（横幅展示用）：
// ListenAvailable 成功返回后置位，true = v4+v6 双栈，false = 仅 v4
var ListenedDualStack bool

// ipv6Supported 探测主机是否支持 IPv6（绑一次 v6 环回，结果缓存）
var ipv6Supported = sync.OnceValue(func() bool {
	l, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		return false
	}
	l.Close()
	return true
})

// ListenAvailable 从 basePort 开始逐个尝试监听，返回第一个可用端口。
// 提前绑定好 listener 再交给 fileServer，调用方（启动横幅）才能拿到真实端口。
// 双栈（公网化支柱 B）：每个端口显式绑 v4（0.0.0.0）与 v6（[::]，V6ONLY）
// 两个套接字，双绑同端口成功才算可用——不用 "tcp" 通配双栈套接字，macOS 上
// 它会与已被占用的 IPv4 端口"共存"，客户端 v4 拨号连到别的程序，破坏端口
// 扫描模型。主机无 v6 时退回仅 v4，照常服务
func ListenAvailable(basePort, count int) (net.Listener, int, error) {
	var lastErr error
	for port := basePort; port < basePort+count; port++ {
		l4, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			lastErr = err
			continue
		}
		if !ipv6Supported() {
			if port != basePort {
				log.Warnf("port %d unavailable, using %d", basePort, port)
			}
			ListenedDualStack = false
			return l4, port, nil
		}
		l6, err := net.Listen("tcp6", fmt.Sprintf("[::]:%d", port))
		if err != nil {
			// v6 侧被占：两栈必须同端口（客户端只扫一个端口号），放弃该端口
			l4.Close()
			lastErr = err
			continue
		}
		if port != basePort {
			log.Warnf("port %d unavailable, using %d", basePort, port)
		}
		ListenedDualStack = true
		return newMultiListener(l4, l6), port, nil
	}
	return nil, 0, fmt.Errorf("no free port in range %d-%d: %w", basePort, basePort+count-1, lastErr)
}

// multiListener 把多个 listener 合并成一个：每个底层 listener 一个 accept
// 协程，连接汇入同一通道。Close 关闭全部底层 listener 并让 Accept 返回
type multiListener struct {
	listeners []net.Listener
	results   chan acceptResult
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

type acceptResult struct {
	conn net.Conn
	err  error
}

func newMultiListener(listeners ...net.Listener) *multiListener {
	m := &multiListener{
		listeners: listeners,
		results:   make(chan acceptResult),
		done:      make(chan struct{}),
	}
	for _, l := range listeners {
		go func(l net.Listener) {
			for {
				conn, err := l.Accept()
				select {
				case m.results <- acceptResult{conn, err}:
				case <-m.done:
					if conn != nil {
						conn.Close()
					}
					return
				}
				if err != nil {
					return
				}
			}
		}(l)
	}
	return m
}

func (m *multiListener) Accept() (net.Conn, error) {
	select {
	case r := <-m.results:
		return r.conn, r.err
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *multiListener) Close() error {
	m.closeOnce.Do(func() {
		close(m.done)
		for _, l := range m.listeners {
			if err := l.Close(); err != nil && m.closeErr == nil {
				m.closeErr = err
			}
		}
	})
	return m.closeErr
}

// Addr 返回 v4 侧地址（横幅与日志以此为准）
func (m *multiListener) Addr() net.Addr { return m.listeners[0].Addr() }
