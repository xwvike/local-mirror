package network

import (
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// freePort 找一个当前空闲的 TCP 端口（存在竞态，测试用途足够）
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// dialAndAccept 从 addr 拨入并确认 listener 能收到这条连接
func dialAndAccept(t *testing.T, l net.Listener, addr string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if conn != nil {
			conn.Close()
		}
		done <- err
	}()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	conn.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("accept for %s: %v", addr, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("accept for %s timed out", addr)
	}
}

// TestListenAvailableDualStack 双栈监听：v4 与 v6 环回都能拨入同一端口
func TestListenAvailableDualStack(t *testing.T) {
	if !ipv6Supported() {
		t.Skip("host has no IPv6")
	}
	base := freePort(t)
	l, port, err := ListenAvailable(base, 1)
	if err != nil {
		t.Fatalf("ListenAvailable: %v", err)
	}
	defer l.Close()
	if port != base {
		t.Fatalf("got port %d, want %d", port, base)
	}
	if !ListenedDualStack {
		t.Fatal("ListenedDualStack should be true on an IPv6-capable host")
	}
	dialAndAccept(t, l, fmt.Sprintf("127.0.0.1:%d", port))
	dialAndAccept(t, l, fmt.Sprintf("[::1]:%d", port))
}

// TestListenAvailableSkipsV4TakenPort v4 侧被占时整个端口跳过——这正是
// 旧实现强制 tcp4 防御的"双栈套接字与被占 v4 端口共存"问题的双栈版答案
func TestListenAvailableSkipsV4TakenPort(t *testing.T) {
	base := freePort(t)
	squatter, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", base))
	if err != nil {
		t.Skipf("cannot squat port %d: %v", base, err)
	}
	defer squatter.Close()

	l, port, err := ListenAvailable(base, 2)
	if err != nil {
		t.Fatalf("ListenAvailable: %v", err)
	}
	defer l.Close()
	if port != base+1 {
		t.Fatalf("got port %d, want %d (v4-taken port must be skipped entirely)", port, base+1)
	}
}

// TestListenAvailableSkipsV6TakenPort v6 侧被占同样跳过整个端口：
// 客户端只扫一个端口号，两栈必须同端口
func TestListenAvailableSkipsV6TakenPort(t *testing.T) {
	if !ipv6Supported() {
		t.Skip("host has no IPv6")
	}
	base := freePort(t)
	squatter, err := net.Listen("tcp6", fmt.Sprintf("[::]:%d", base))
	if err != nil {
		t.Skipf("cannot squat v6 port %d: %v", base, err)
	}
	defer squatter.Close()

	l, port, err := ListenAvailable(base, 2)
	if err != nil {
		t.Fatalf("ListenAvailable: %v", err)
	}
	defer l.Close()
	if port != base+1 {
		t.Fatalf("got port %d, want %d (v6-taken port must be skipped entirely)", port, base+1)
	}
}

// TestMultiListenerClose Close 后 Accept 立即返回错误，不悬挂
func TestMultiListenerClose(t *testing.T) {
	if !ipv6Supported() {
		t.Skip("host has no IPv6")
	}
	base := freePort(t)
	l, _, err := ListenAvailable(base, 1)
	if err != nil {
		t.Fatalf("ListenAvailable: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Accept after Close should return an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept after Close hung")
	}
}

// flakyListener 第一次 Accept 返回一个非 ErrClosed 的临时错误
// （模拟 EMFILE/ENOBUFS——Go 不内部重试、会漏到 Accept 上来的那类），
// 之后恢复正常
type flakyListener struct {
	net.Listener
	failed atomic.Bool
}

func (f *flakyListener) Accept() (net.Conn, error) {
	if f.failed.CompareAndSwap(false, true) {
		return nil, errors.New("simulated transient accept error")
	}
	return f.Listener.Accept()
}

// TestAcceptRetriesTransientError 临时 accept 错误后该侧必须继续服务。
// 回归：旧实现里 accept 协程遇错即退，套接字仍绑着却再没人 accept——
// 客户端能完成 TCP 三次握手但收不到任何字节，只能等到自己的握手超时，
// 而另一侧（另一地址族）照常工作，进程看着完全健康
func TestAcceptRetriesTransientError(t *testing.T) {
	inner, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	flaky := &flakyListener{Listener: inner}
	m := newMultiListener(flaky)
	defer m.Close()

	// 第一次 Accept 吃掉注入的错误后，这一侧仍须能交出真实连接
	dialAndAccept(t, m, flaky.Addr().String())
}

// TestAcceptSurvivesErrorOnOneStack 双栈里一侧抖过之后，两侧都还在服务
func TestAcceptSurvivesErrorOnOneStack(t *testing.T) {
	if !ipv6Supported() {
		t.Skip("host has no IPv6")
	}
	port := freePort(t)
	l4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("v4 port %d taken: %v", port, err)
	}
	l6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	if err != nil {
		l4.Close()
		t.Skipf("v6 port %d taken: %v", port, err)
	}
	m := newMultiListener(l4, &flakyListener{Listener: l6})
	defer m.Close()

	// v6 侧注入的错误被吃掉后，v6 与 v4 都必须照常 accept
	dialAndAccept(t, m, fmt.Sprintf("[::1]:%d", port))
	dialAndAccept(t, m, fmt.Sprintf("127.0.0.1:%d", port))
}

// deadListener 的 Accept 恒返回 ErrClosed，模拟底层套接字已没了
type deadListener struct{ net.Listener }

func (d deadListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }

// TestAcceptReportsAllLoopsGone 所有 accept 循环都没了时，Accept 必须
// 立刻返回 ErrNoListenerLeft，让上层响亮地退出。
// 回归：旧实现会永久阻塞在 <-results 上，服务端静默挂死，既不报错也不退出
func TestAcceptReportsAllLoopsGone(t *testing.T) {
	inner, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	m := newMultiListener(deadListener{inner})
	defer m.Close()

	done := make(chan error, 1)
	go func() {
		_, err := m.Accept()
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoListenerLeft) {
			t.Fatalf("want ErrNoListenerLeft, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Accept blocked forever instead of reporting that nothing is accepting")
	}
}

// TestValidatePeerAddr --connect 的地址写法校验
func TestValidatePeerAddr(t *testing.T) {
	ok := []string{
		"",                             // 空 = 局域网发现，由别处校验
		"192.168.1.9",                  // v4
		"192.168.1.9:52345",            // v4 带端口
		"example.com",                  // 域名
		"example.com:52345",            // 域名带端口
		"::1",                          // v6 裸字面量
		"2001:db8::1",                  // v6 全局
		"[2001:db8::1]:52345",          // v6 带端口（正确写法）
		"fe80::8fb:a322:2012:d191%en0", // 链路本地带 zone
		"[fe80::1%en0]:52345",          // 链路本地带 zone 与端口
	}
	for _, addr := range ok {
		if err := ValidatePeerAddr(addr); err != nil {
			t.Errorf("ValidatePeerAddr(%q) = %v, want nil", addr, err)
		}
	}
	bad := []string{
		"2001:db8::1:52345",                            // v6 带端口未加方括号
		"2408:8220:371:b3d0:1859:fff7:b576:8f43:52345", // 同上，9 段
		"not:an:address",                               // 冒号但不是 v6
	}
	for _, addr := range bad {
		if err := ValidatePeerAddr(addr); err == nil {
			t.Errorf("ValidatePeerAddr(%q) = nil, want an error", addr)
		}
	}
}
