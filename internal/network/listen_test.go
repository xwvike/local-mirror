package network

import (
	"fmt"
	"net"
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
