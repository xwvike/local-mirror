package network

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"local-mirror/config"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	log "github.com/sirupsen/logrus"
	"github.com/zeebo/blake3"
)

// UDP 局域网服务发现，查询-应答式：客户端向组播组/广播地址发探测包，
// 在线的服务端单播应答自己的 TCP 端口、别名与同步目录。
// 选择查询-应答而非周期广播：空闲时零流量、零唤醒（本项目明确关注休眠耗电）。
//
// 安全模型：设置 -k 口令时探测与应答都带 keyed MAC，服务端静默忽略
// 未认证探测（不向局域网扫描者泄露同步路径）。应答本身是明文，对
// 被动嗅探者可见——嗅探者本就能在一次合法发现交互中看到同样内容，
// 属可接受范围。MAC 过的探测可被重放，只会诱出应答，无权限提升。
const (
	// DiscoveryMagic 发现协议魔数，与 TCP 协议的 MagicNumber 区分
	DiscoveryMagic uint32 = 0xD15C4FBE
	// DiscoveryPort 发现协议的 UDP 端口。多实例经 ListenMulticastUDP 的
	// SO_REUSEADDR 共享监听，无需像 TCP 那样逐实例递增
	DiscoveryPort = config.DefaultPort

	discoveryKindProbe byte = 0x01
	discoveryKindReply byte = 0x02

	// DiscoveryMaxAlias/DiscoveryMaxPath 应答中字符串的字节上限，
	// 保证整包 ≤610 字节，避免 IP 分片
	DiscoveryMaxAlias = 64
	DiscoveryMaxPath  = 512

	discoveryMACLen = 16
	// 探测包定长：magic(4)+kind(1)+version(2)+clientID(4)+nonce(8)+authFlag(1)+mac(16)
	discoveryProbeLen = 36
	// 应答包头部：magic(4)+kind(1)+version(2)+serverID(4)+tcpPort(2)+role(1)+authFlag(1)+aliasLen(1)+pathLen(2)
	discoveryReplyHeaderLen = 18
)

// discoveryGroup 组播组地址（239.255.0.0/16 站点本地范围）
var discoveryGroup = net.IPv4(239, 255, 77, 77)

// DiscoveredServer 一次扫描发现的服务端
type DiscoveredServer struct {
	InstanceID uint32
	IP         string // 取自应答包的 UDP 源地址（即客户端可达的地址）
	TCPPort    uint16
	Role       uint8
	Alias      string
	SyncPath   string
}

func (d DiscoveredServer) Addr() string {
	return net.JoinHostPort(d.IP, strconv.Itoa(int(d.TCPPort)))
}

// deriveDiscoveryKey 从口令派生发现协议的 MAC 密钥。
// 域分离前缀与 DerivePSK 不同，两个协议的密钥互不相通
func deriveDiscoveryKey(secret string) []byte {
	sum := blake3.Sum256([]byte("local-mirror-discovery-mac-v1:" + secret))
	return sum[:]
}

func discoveryMAC(key []byte, parts ...[]byte) []byte {
	h, err := blake3.NewKeyed(key)
	if err != nil {
		// key 恒为 32 字节（blake3.Sum256 输出），此处不可达
		panic(fmt.Sprintf("discovery MAC key: %v", err))
	}
	for _, p := range parts {
		_, _ = h.Write(p)
	}
	return h.Sum(nil)[:discoveryMACLen]
}

// truncateUTF8 把 s 截断到不超过 maxBytes 字节，不切坏多字节字符
func truncateUTF8(s string, maxBytes int) []byte {
	b := []byte(s)
	if len(b) <= maxBytes {
		return b
	}
	for i := maxBytes; i > 0 && i > maxBytes-utf8.UTFMax; i-- {
		if utf8.Valid(b[:i]) {
			return b[:i]
		}
	}
	return nil
}

// ---- 编解码（纯函数，不依赖网络） ----

type discoveryProbe struct {
	Version  uint16
	ClientID uint32
	Nonce    [8]byte
	Authed   bool
}

func encodeProbe(version uint16, clientID uint32, nonce [8]byte, key []byte) []byte {
	buf := make([]byte, discoveryProbeLen)
	binary.BigEndian.PutUint32(buf[0:4], DiscoveryMagic)
	buf[4] = discoveryKindProbe
	binary.BigEndian.PutUint16(buf[5:7], version)
	binary.BigEndian.PutUint32(buf[7:11], clientID)
	copy(buf[11:19], nonce[:])
	if key != nil {
		buf[19] = 1
		copy(buf[20:], discoveryMAC(key, buf[:20]))
	}
	return buf
}

// decodeProbe 严格校验长度/魔数/类型；MAC 校验由调用方（handleProbe）
// 结合本端是否配置口令决定
func decodeProbe(pkt []byte) (discoveryProbe, error) {
	var p discoveryProbe
	if len(pkt) != discoveryProbeLen {
		return p, fmt.Errorf("probe length %d != %d", len(pkt), discoveryProbeLen)
	}
	if binary.BigEndian.Uint32(pkt[0:4]) != DiscoveryMagic {
		return p, fmt.Errorf("bad probe magic")
	}
	if pkt[4] != discoveryKindProbe {
		return p, fmt.Errorf("bad probe kind %d", pkt[4])
	}
	p.Version = binary.BigEndian.Uint16(pkt[5:7])
	p.ClientID = binary.BigEndian.Uint32(pkt[7:11])
	copy(p.Nonce[:], pkt[11:19])
	p.Authed = pkt[19] == 1
	return p, nil
}

// encodeResponse 编码应答包。MAC 覆盖 nonce||头部与正文，
// 把应答绑定到本次探测（防止跨扫描串包）
func encodeResponse(version uint16, nonce [8]byte, r DiscoveredServer, key []byte) []byte {
	alias := truncateUTF8(r.Alias, DiscoveryMaxAlias)
	path := truncateUTF8(r.SyncPath, DiscoveryMaxPath)
	body := make([]byte, discoveryReplyHeaderLen+len(alias)+len(path))
	binary.BigEndian.PutUint32(body[0:4], DiscoveryMagic)
	body[4] = discoveryKindReply
	binary.BigEndian.PutUint16(body[5:7], version)
	binary.BigEndian.PutUint32(body[7:11], r.InstanceID)
	binary.BigEndian.PutUint16(body[11:13], r.TCPPort)
	body[13] = r.Role
	if key != nil {
		body[14] = 1
	}
	body[15] = byte(len(alias))
	binary.BigEndian.PutUint16(body[16:18], uint16(len(path)))
	copy(body[18:], alias)
	copy(body[18+len(alias):], path)

	mac := make([]byte, discoveryMACLen)
	if key != nil {
		copy(mac, discoveryMAC(key, nonce[:], body))
	}
	return append(body, mac...)
}

// parseResponse 解析并校验应答。key 非空时要求应答已认证且 MAC 匹配。
// 不含 IP 字段（由调用方从 UDP 源地址填入）
func parseResponse(pkt []byte, nonce [8]byte, key []byte, wantVersion uint16) (DiscoveredServer, error) {
	var r DiscoveredServer
	if len(pkt) < discoveryReplyHeaderLen+discoveryMACLen {
		return r, fmt.Errorf("reply too short: %d", len(pkt))
	}
	if binary.BigEndian.Uint32(pkt[0:4]) != DiscoveryMagic {
		return r, fmt.Errorf("bad reply magic")
	}
	if pkt[4] != discoveryKindReply {
		return r, fmt.Errorf("bad reply kind %d", pkt[4])
	}
	if v := binary.BigEndian.Uint16(pkt[5:7]); v != wantVersion {
		return r, fmt.Errorf("version mismatch: got %d want %d", v, wantVersion)
	}
	aliasLen := int(pkt[15])
	pathLen := int(binary.BigEndian.Uint16(pkt[16:18]))
	if aliasLen > DiscoveryMaxAlias || pathLen > DiscoveryMaxPath {
		return r, fmt.Errorf("reply field length out of bounds: alias=%d path=%d", aliasLen, pathLen)
	}
	if len(pkt) != discoveryReplyHeaderLen+aliasLen+pathLen+discoveryMACLen {
		return r, fmt.Errorf("reply length %d inconsistent with fields", len(pkt))
	}
	body := pkt[:len(pkt)-discoveryMACLen]
	mac := pkt[len(pkt)-discoveryMACLen:]
	if key != nil {
		if pkt[14] != 1 {
			return r, fmt.Errorf("unauthenticated reply while secret is set")
		}
		if subtle.ConstantTimeCompare(mac, discoveryMAC(key, nonce[:], body)) != 1 {
			return r, fmt.Errorf("reply MAC mismatch")
		}
	}
	r.InstanceID = binary.BigEndian.Uint32(pkt[7:11])
	r.TCPPort = binary.BigEndian.Uint16(pkt[11:13])
	r.Role = pkt[13]
	r.Alias = string(pkt[18 : 18+aliasLen])
	r.SyncPath = string(pkt[18+aliasLen : 18+aliasLen+pathLen])
	return r, nil
}

// handleProbe 服务端处理一个探测包，返回应答字节（ok=false 表示静默丢弃）。
// 纯函数，便于不起网络的单元测试
func handleProbe(pkt []byte, version uint16, self DiscoveredServer, key []byte) ([]byte, bool) {
	probe, err := decodeProbe(pkt)
	if err != nil {
		return nil, false
	}
	// 版本不符静默丢弃：不同版本的包布局可能不同，跨版本发现无意义，
	// -r 直连是升级期间的逃生通道
	if probe.Version != version {
		return nil, false
	}
	// 自己的探测（中继扫描上游时收到自己实例的包）不应答
	if probe.ClientID == self.InstanceID {
		return nil, false
	}
	if key != nil {
		if !probe.Authed {
			log.Debugf("忽略未认证的发现探测")
			return nil, false
		}
		if subtle.ConstantTimeCompare(pkt[20:36], discoveryMAC(key, pkt[:20])) != 1 {
			log.Debugf("忽略 MAC 不匹配的发现探测")
			return nil, false
		}
	}
	return encodeResponse(version, probe.Nonce, self, key), true
}

// ---- 服务端应答器 ----

// StartDiscoveryResponder 启动 UDP 发现应答器。
// ListenMulticastUDP(ifi=nil) 只在系统默认接口入组，多网卡（有线+无线）
// 会漏收另一块网卡上的探测，因此逐接口各建一个 socket（SO_REUSEADDR
// 使同端口多 socket 合法，副作用是一次组播/广播探测会触发每个 socket
// 各应答一次——客户端按 InstanceID 去重是主逻辑而非防御）。
// wildcard 绑定使这些 socket 同时能收到发往本端口的广播与单播包。
// 返回的 stop 关闭全部 socket；进程退出时 OS 自动回收，可不调用
func StartDiscoveryResponder(tcpPort int, alias, syncPath, secret string) (func(), error) {
	var key []byte
	if secret != "" {
		key = deriveDiscoveryKey(secret)
	}
	self := DiscoveredServer{
		InstanceID: config.InstanceID,
		TCPPort:    uint16(tcpPort),
		Role:       config.ModeMap[*config.Mode],
		Alias:      alias,
		SyncPath:   syncPath,
	}
	gaddr := &net.UDPAddr{IP: discoveryGroup, Port: DiscoveryPort}

	var conns []*net.UDPConn
	ifis, err := net.Interfaces()
	if err == nil {
		for i := range ifis {
			ifi := ifis[i]
			if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
				continue
			}
			c, err := net.ListenMulticastUDP("udp4", &ifi, gaddr)
			if err != nil {
				log.Debugf("发现应答器: 接口 %s 入组失败: %v", ifi.Name, err)
				continue
			}
			conns = append(conns, c)
		}
	}
	if len(conns) == 0 {
		// 退回默认接口入组，仍然失败则报给调用方（非致命，可 -r 直连）
		c, err := net.ListenMulticastUDP("udp4", nil, gaddr)
		if err != nil {
			return nil, fmt.Errorf("加入发现组播组失败: %w", err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		go discoveryRespondLoop(c, self, key)
	}
	log.Infof("UDP 服务发现应答器已启动（%d 个接口，端口 %d）", len(conns), DiscoveryPort)
	return func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}, nil
}

func discoveryRespondLoop(conn *net.UDPConn, self DiscoveredServer, key []byte) {
	buf := make([]byte, 128)
	for {
		n, raddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			return // socket 已被 stop() 关闭
		}
		reply, ok := handleProbe(buf[:n], config.ProtocolVersion, self, key)
		if !ok {
			continue
		}
		if _, err := conn.WriteToUDP(reply, raddr); err != nil {
			log.Debugf("发现应答发送失败 %s: %v", raddr, err)
		}
	}
}

// ---- 客户端扫描器 ----

// probeSender 一个探测发送 socket 及其目标地址集；
// socket 同时用于接收单播回来的应答
type probeSender struct {
	conn    *net.UDPConn
	targets []*net.UDPAddr
}

// DiscoverServers 扫描局域网内的服务端：向组播组、各接口的子网定向广播
// 地址及 127.0.0.1 发送探测，收集应答直到超时。按 InstanceID 去重，
// 过滤自身（selfID，中继场景）与协议版本不符的应答。
// 未发现任何服务端返回空切片和 nil 错误
func DiscoverServers(timeout time.Duration, secret string, selfID uint32) ([]DiscoveredServer, error) {
	senders, err := buildProbeSenders()
	if err != nil {
		return nil, err
	}
	return discoverOn(senders, timeout, secret, selfID)
}

// buildProbeSenders 逐接口建发送 socket：绑定接口 IPv4 保证广播从该接口
// 发出（255.255.255.255 只走默认路由接口，必须用子网定向广播 + 绑定源地址），
// Control 里设 SO_BROADCAST（Go 默认不设，直接发广播会 EPERM）与
// IP_MULTICAST_IF（组播发送接口跟随绑定的接口）。
// 另建一个普通回环 socket 探测 127.0.0.1（本机离线场景）
func buildProbeSenders() ([]probeSender, error) {
	var senders []probeSender

	ifis, err := net.Interfaces()
	if err == nil {
		for _, ifi := range ifis {
			if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
				continue
			}
			if ifi.Flags&(net.FlagBroadcast|net.FlagMulticast) == 0 {
				continue
			}
			addrs, err := ifi.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				ipnet, ok := a.(*net.IPNet)
				if !ok || ipnet.IP.To4() == nil {
					continue
				}
				conn, err := listenProbeUDP(ipnet.IP.To4())
				if err != nil {
					log.Debugf("发现扫描: 接口 %s (%s) 建 socket 失败: %v", ifi.Name, ipnet.IP, err)
					continue
				}
				targets := []*net.UDPAddr{{IP: discoveryGroup, Port: DiscoveryPort}}
				if bc := subnetBroadcast(ipnet); bc != nil {
					targets = append(targets, &net.UDPAddr{IP: bc, Port: DiscoveryPort})
				}
				senders = append(senders, probeSender{conn: conn, targets: targets})
				break // 每接口一个 v4 地址足够
			}
		}
	}

	// 回环探测：本机服务端场景（含单机测试）。普通 socket 即可
	if lo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}); err == nil {
		senders = append(senders, probeSender{
			conn:    lo,
			targets: []*net.UDPAddr{{IP: net.IPv4(127, 0, 0, 1), Port: DiscoveryPort}},
		})
	}

	if len(senders) == 0 {
		return nil, fmt.Errorf("没有可用于发现扫描的网络接口")
	}
	return senders, nil
}

// subnetBroadcast 计算 IPv4 子网定向广播地址（主机位全 1）
func subnetBroadcast(ipnet *net.IPNet) net.IP {
	ip4 := ipnet.IP.To4()
	mask := ipnet.Mask
	if len(mask) == 16 {
		mask = mask[12:]
	}
	if ip4 == nil || len(mask) != 4 {
		return nil
	}
	bc := make(net.IP, 4)
	for i := range bc {
		bc[i] = ip4[i] | ^mask[i]
	}
	return bc
}

// discoverOn 在给定的发送 socket 集上执行一轮扫描（senders 的所有 socket
// 都会被关闭）。与 socket 构建分离，便于测试用回环 socket 直接驱动
func discoverOn(senders []probeSender, timeout time.Duration, secret string, selfID uint32) ([]DiscoveredServer, error) {
	var key []byte
	if secret != "" {
		key = deriveDiscoveryKey(secret)
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("生成发现 nonce 失败: %w", err)
	}
	probe := encodeProbe(config.ProtocolVersion, selfID, nonce, key)
	deadline := time.Now().Add(timeout)

	sendAll := func() {
		for _, s := range senders {
			for _, t := range s.targets {
				if _, err := s.conn.WriteToUDP(probe, t); err != nil {
					log.Debugf("发现探测发送失败 %s: %v", t, err)
				}
			}
		}
	}
	sendAll()
	// UDP 无重传，半程补发一次以抵抗单包丢失；socket 若已关闭仅静默失败
	retransmit := time.AfterFunc(timeout/2, sendAll)
	defer retransmit.Stop()

	resCh := make(chan DiscoveredServer, 32)
	var wg sync.WaitGroup
	for _, s := range senders {
		wg.Add(1)
		go func(c *net.UDPConn) {
			defer wg.Done()
			defer c.Close()
			_ = c.SetReadDeadline(deadline)
			buf := make([]byte, 2048)
			for {
				n, raddr, err := c.ReadFromUDP(buf)
				if err != nil {
					return // 超时或关闭
				}
				srv, err := parseResponse(buf[:n], nonce, key, config.ProtocolVersion)
				if err != nil {
					log.Debugf("忽略无效发现应答（来自 %s）: %v", raddr, err)
					continue
				}
				srv.IP = raddr.IP.String()
				resCh <- srv
			}
		}(s.conn)
	}
	go func() {
		wg.Wait()
		close(resCh)
	}()

	found := make(map[uint32]DiscoveredServer)
	for srv := range resCh {
		if srv.InstanceID == selfID {
			continue
		}
		if _, ok := found[srv.InstanceID]; !ok {
			found[srv.InstanceID] = srv
		}
	}
	result := make([]DiscoveredServer, 0, len(found))
	for _, srv := range found {
		result = append(result, srv)
	}
	// 稳定排序，UI 列表与非 TTY 报错输出的顺序可复现
	sort.Slice(result, func(i, j int) bool {
		if result[i].IP != result[j].IP {
			return result[i].IP < result[j].IP
		}
		return result[i].TCPPort < result[j].TCPPort
	})
	return result, nil
}
