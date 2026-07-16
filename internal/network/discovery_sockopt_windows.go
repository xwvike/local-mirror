//go:build windows

package network

import "net"

// listenProbeUDP Windows 降级实现：不设 SO_BROADCAST/IP_MULTICAST_IF，
// 组播/子网广播可能只从默认接口发出，发现失败时可用 -r 直连
func listenProbeUDP(ifIP net.IP) (*net.UDPConn, error) {
	addr := &net.UDPAddr{IP: ifIP.To4()}
	return net.ListenUDP("udp4", addr)
}
