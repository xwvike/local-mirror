package network

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"local-mirror/internal/status"

	log "github.com/sirupsen/logrus"
)

// ListenedDualStack 服务端实际监听栈（横幅展示用）：
// ListenAvailable 成功返回后置位，true = v4+v6 双栈，false = 仅 v4
var ListenedDualStack bool

// ipv6Supported 探测主机是否支持 IPv6（绑一次 v6 通配地址，结果缓存）。
// 探针用 [::]:0 而非 [::1]:0——后者在「环回关了 v6、网卡仍有全局 v6」的
// 容器/定制内核上会假阴性，让本可双栈的主机退成仅 v4
var ipv6Supported = sync.OnceValue(func() bool {
	l, err := net.Listen("tcp6", "[::]:0")
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
// 扫描模型。主机无 v6 时退回仅 v4，照常服务。
// 单栈也包进 multiListener：accept 错误的处置只有一条代码路径
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
			return newMultiListener(l4), port, nil
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

// accept 临时错误的退避区间。Go 内部只重试 EINTR/EAGAIN/ECONNABORTED，
// EMFILE/ENFILE（fd 耗尽）、ENOBUFS 等会直接漏到 Accept 上来；它们都是
// 会自行缓解的瞬时状态，退避重试即可，不该让这一侧永久停服
const (
	acceptRetryBase = 10 * time.Millisecond
	acceptRetryMax  = time.Second
)

// ErrNoListenerLeft 所有 accept 循环都退出了（且不是因为 Close）。
// 此时套接字可能仍绑着但无人 accept，属于不可恢复的服务端状态：
// 交由上层响亮地退出，而不是让 Accept 永久阻塞成静默挂死
var ErrNoListenerLeft = errors.New("no listener left accepting connections")

// multiListener 把多个 listener 合并成一个：每个底层 listener 一个 accept
// 协程，连接汇入同一通道。Close 关闭全部底层 listener 并让 Accept 返回。
//
// 错误语义是双栈可用性的要害：底层 Accept 的临时错误绝不能让该侧协程退出。
// 套接字仍绑着，内核照常完成三次握手（外部探测看着端口是通的），但再没人
// accept——客户端连上后收不到任何字节，只能等到自己的握手超时；另一侧照常
// 工作，于是进程看着活着、横幅照常写着 IPv4 + IPv6，实际只有一个地址族在
// 服务。因此临时错误在本协程内退避重试，只有本 listener 被关（ErrClosed）
// 或 multiListener 整体关闭才退出；全部退出而非因 Close 时 Accept 返回
// ErrNoListenerLeft
type multiListener struct {
	listeners []net.Listener
	results   chan net.Conn
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
	live      atomic.Int32  // 仍在 accept 的协程数
	allDead   chan struct{} // live 归零时关闭
	// liveFlags 与 listeners 一一对应：该侧 accept 循环是否还在收。
	// 供 status 探针读出"半边停服"——这是本项目出过的真实故障，而套接字
	// 仍绑着，外部端口探测与启动横幅都看不出来
	liveFlags []atomic.Bool
}

// families 各地址族的 accept 存活，供 status 探针调用。
// 只读原子标志：status 在持自己的锁时调用本函数，这里不能加锁也不能回调它
func (m *multiListener) families() []status.ListenFamily {
	out := make([]status.ListenFamily, 0, len(m.listeners))
	for i, l := range m.listeners {
		addr := l.Addr()
		out = append(out, status.ListenFamily{
			Family: addrFamily(addr),
			Addr:   addr.String(),
			Live:   m.liveFlags[i].Load(),
		})
	}
	return out
}

// addrFamily 监听地址属于哪个地址族。0.0.0.0 的 To4 非空、[::] 为空
func addrFamily(a net.Addr) string {
	if ta, ok := a.(*net.TCPAddr); ok && ta.IP.To4() != nil {
		return "IPv4"
	}
	return "IPv6"
}

func newMultiListener(listeners ...net.Listener) *multiListener {
	m := &multiListener{
		listeners: listeners,
		results:   make(chan net.Conn),
		done:      make(chan struct{}),
		allDead:   make(chan struct{}),
	}
	m.live.Store(int32(len(listeners)))
	m.liveFlags = make([]atomic.Bool, len(listeners))
	for i, l := range listeners {
		m.liveFlags[i].Store(true)
		go m.acceptLoop(i, l)
	}
	// 注册监听存活探针：--status 与前台状态区据此显示哪个地址族还在服务
	status.RegisterListenProbe(m.families)
	return m
}

// acceptLoop 单个底层 listener 的 accept 循环
func (m *multiListener) acceptLoop(idx int, l net.Listener) {
	defer func() {
		m.liveFlags[idx].Store(false)
		remaining := m.live.Add(-1)
		select {
		case <-m.done: // 整体关闭中，正常收尾
		default:
			// 半边停服是最难察觉的故障态，按 error 级记下来（会落日志文件）
			log.Errorf("accept loop for %s exited: that address family is no longer served (%d listener(s) still accepting)",
				l.Addr(), remaining)
		}
		if remaining == 0 {
			close(m.allDead)
		}
	}()

	delay := acceptRetryBase
	for {
		conn, err := l.Accept()
		if err != nil {
			// 本 listener 已关（含整体 Close）：正常退出
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-m.done:
				return
			default:
			}
			log.Errorf("accept on %s failed: %v (retrying in %v)", l.Addr(), err, delay)
			select {
			case <-time.After(delay):
			case <-m.done:
				return
			}
			delay = min(delay*2, acceptRetryMax)
			continue
		}
		delay = acceptRetryBase
		select {
		case m.results <- conn:
		case <-m.done:
			conn.Close()
			return
		}
	}
}

func (m *multiListener) Accept() (net.Conn, error) {
	select {
	case conn := <-m.results:
		return conn, nil
	case <-m.done:
		return nil, net.ErrClosed
	case <-m.allDead:
		// Close 走的也是这条路（协程退出后 live 归零），按正常关停回报
		select {
		case <-m.done:
			return nil, net.ErrClosed
		default:
		}
		return nil, fmt.Errorf("%w (%d bound, none accepting)", ErrNoListenerLeft, len(m.listeners))
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

// Addr 返回首个（双栈时为 v4）底层地址（横幅与日志以此为准）
func (m *multiListener) Addr() net.Addr { return m.listeners[0].Addr() }
