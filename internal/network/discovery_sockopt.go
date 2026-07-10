//go:build !windows

package network

import (
	"context"
	"net"
	"syscall"
)

// listenProbeUDP 建立一个绑定到指定接口 IPv4 的探测发送 socket。
// Go 默认不设 SO_BROADCAST，直接向广播地址发包会 EPERM/EACCES；
// IP_MULTICAST_IF 指定组播从绑定的接口发出（否则只走系统默认组播接口）
func listenProbeUDP(ifIP net.IP) (*net.UDPConn, error) {
	var addr4 [4]byte
	copy(addr4[:], ifIP.To4())

	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var serr error
			err := c.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
				if serr != nil {
					return
				}
				serr = syscall.SetsockoptInet4Addr(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF, addr4)
			})
			if err != nil {
				return err
			}
			return serr
		},
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", net.JoinHostPort(ifIP.String(), "0"))
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}
