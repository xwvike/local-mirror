package network

import (
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"local-mirror/config"
)

var testServer = DiscoveredServer{
	InstanceID: 0xAABBCCDD,
	TCPPort:    52346,
	Role:       0x01,
	Alias:      "调度机-mac",
	SyncPath:   "/Users/测试/图片库",
}

func testNonce() [8]byte { return [8]byte{1, 2, 3, 4, 5, 6, 7, 8} }

func TestProbeRoundTrip(t *testing.T) {
	for _, key := range [][]byte{nil, deriveDiscoveryKey("s1")} {
		pkt := encodeProbe(0x0002, 0x11223344, testNonce(), key)
		if len(pkt) != discoveryProbeLen {
			t.Fatalf("probe length = %d, want %d", len(pkt), discoveryProbeLen)
		}
		p, err := decodeProbe(pkt)
		if err != nil {
			t.Fatalf("decodeProbe: %v", err)
		}
		if p.Version != 0x0002 || p.ClientID != 0x11223344 || p.Nonce != testNonce() {
			t.Errorf("round-trip mismatch: %+v", p)
		}
		if p.Authed != (key != nil) {
			t.Errorf("Authed = %v, key present = %v", p.Authed, key != nil)
		}
	}
}

func TestResponseRoundTrip(t *testing.T) {
	for _, key := range [][]byte{nil, deriveDiscoveryKey("s1")} {
		pkt := encodeResponse(0x0002, testNonce(), testServer, key)
		r, err := parseResponse(pkt, testNonce(), key, 0x0002)
		if err != nil {
			t.Fatalf("parseResponse: %v", err)
		}
		if r.InstanceID != testServer.InstanceID || r.TCPPort != testServer.TCPPort ||
			r.Role != testServer.Role || r.Alias != testServer.Alias || r.SyncPath != testServer.SyncPath {
			t.Errorf("round-trip mismatch: %+v", r)
		}
	}
}

func TestResponseMACTamper(t *testing.T) {
	key := deriveDiscoveryKey("s1")
	pkt := encodeResponse(0x0002, testNonce(), testServer, key)
	// 除魔数/类型/长度字段外，翻转任何一个字节都必须导致 MAC 校验失败
	for i := 5; i < len(pkt); i++ {
		tampered := make([]byte, len(pkt))
		copy(tampered, pkt)
		tampered[i] ^= 0x01
		if _, err := parseResponse(tampered, testNonce(), key, 0x0002); err == nil {
			t.Errorf("tampered byte %d accepted", i)
		}
	}
	// nonce 不匹配（跨扫描串包）也必须拒绝
	if _, err := parseResponse(pkt, [8]byte{9, 9, 9, 9, 9, 9, 9, 9}, key, 0x0002); err == nil {
		t.Error("reply bound to different nonce accepted")
	}
}

func TestSecretGating(t *testing.T) {
	key := deriveDiscoveryKey("s1")
	// 有密钥的服务端忽略未认证探测
	plainProbe := encodeProbe(0x0002, 1, testNonce(), nil)
	if _, ok := handleProbe(plainProbe, 0x0002, testServer, key); ok {
		t.Error("server with secret answered unauthenticated probe")
	}
	// 探测 MAC 被篡改同样忽略
	badProbe := encodeProbe(0x0002, 1, testNonce(), key)
	badProbe[25] ^= 0x01
	if _, ok := handleProbe(badProbe, 0x0002, testServer, key); ok {
		t.Error("server accepted probe with tampered MAC")
	}
	// 口令不同 → MAC 不同 → 忽略
	otherProbe := encodeProbe(0x0002, 1, testNonce(), deriveDiscoveryKey("s2"))
	if _, ok := handleProbe(otherProbe, 0x0002, testServer, key); ok {
		t.Error("server accepted probe MAC'd with different secret")
	}
	// 有密钥的客户端丢弃未认证应答
	plainReply := encodeResponse(0x0002, testNonce(), testServer, nil)
	if _, err := parseResponse(plainReply, testNonce(), key, 0x0002); err == nil {
		t.Error("client with secret accepted unauthenticated reply")
	}
	// 双方口令一致的完整探测-应答链路
	goodProbe := encodeProbe(0x0002, 1, testNonce(), key)
	reply, ok := handleProbe(goodProbe, 0x0002, testServer, key)
	if !ok {
		t.Fatal("server rejected valid authenticated probe")
	}
	if _, err := parseResponse(reply, testNonce(), key, 0x0002); err != nil {
		t.Errorf("client rejected valid authenticated reply: %v", err)
	}
}

func TestHandleProbeFilters(t *testing.T) {
	// 版本不匹配静默丢弃
	probe := encodeProbe(0x0001, 1, testNonce(), nil)
	if _, ok := handleProbe(probe, 0x0002, testServer, nil); ok {
		t.Error("answered version-mismatched probe")
	}
	// 自己实例的探测不应答
	selfProbe := encodeProbe(0x0002, testServer.InstanceID, testNonce(), nil)
	if _, ok := handleProbe(selfProbe, 0x0002, testServer, nil); ok {
		t.Error("answered own probe")
	}
	// 结构错误：长度、魔数、类型
	for _, bad := range [][]byte{
		nil,
		{1, 2, 3},
		make([]byte, discoveryProbeLen+1),
		func() []byte { p := encodeProbe(0x0002, 1, testNonce(), nil); p[0] ^= 0xFF; return p }(),
		func() []byte { p := encodeProbe(0x0002, 1, testNonce(), nil); p[4] = discoveryKindReply; return p }(),
	} {
		if _, ok := handleProbe(bad, 0x0002, testServer, nil); ok {
			t.Errorf("answered malformed probe %v", bad)
		}
	}
}

func TestParseResponseBounds(t *testing.T) {
	good := encodeResponse(0x0002, testNonce(), testServer, nil)
	cases := map[string][]byte{
		"too short":     good[:discoveryReplyHeaderLen+discoveryMACLen-1],
		"bad magic":     func() []byte { p := append([]byte{}, good...); p[0] ^= 0xFF; return p }(),
		"bad kind":      func() []byte { p := append([]byte{}, good...); p[4] = discoveryKindProbe; return p }(),
		"trailing junk": append(append([]byte{}, good...), 0x00),
		"alias len lie": func() []byte { p := append([]byte{}, good...); p[15]++; return p }(),
	}
	for name, pkt := range cases {
		if _, err := parseResponse(pkt, testNonce(), nil, 0x0002); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parseResponse(good, testNonce(), nil, 0x0003); err == nil {
		t.Error("version mismatch accepted")
	}
}

func TestTruncateUTF8(t *testing.T) {
	long := DiscoveredServer{
		InstanceID: 1,
		TCPPort:    1,
		Alias:      strings.Repeat("图", 40),  // 120 字节，超过 64 上限
		SyncPath:   strings.Repeat("库", 200), // 600 字节，超过 512 上限
	}
	pkt := encodeResponse(0x0002, testNonce(), long, nil)
	r, err := parseResponse(pkt, testNonce(), nil, 0x0002)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if !utf8.ValidString(r.Alias) || !utf8.ValidString(r.SyncPath) {
		t.Error("truncation broke UTF-8")
	}
	if len(r.Alias) > DiscoveryMaxAlias || len(r.SyncPath) > DiscoveryMaxPath {
		t.Errorf("lengths exceed caps: alias=%d path=%d", len(r.Alias), len(r.SyncPath))
	}
	if len(r.Alias) == 0 || len(r.SyncPath) == 0 {
		t.Error("truncation produced empty string")
	}
}

// TestDiscoverLoopback 回环上的完整扫描链路：应答重复发送验证去重，
// 混入 selfID 应答验证过滤
func TestDiscoverLoopback(t *testing.T) {
	responder, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen responder: %v", err)
	}
	defer responder.Close()

	selfID := uint32(0x0EADBEEF)
	go func() {
		buf := make([]byte, 128)
		for {
			n, raddr, err := responder.ReadFromUDP(buf)
			if err != nil {
				return
			}
			probe, err := decodeProbe(buf[:n])
			if err != nil {
				continue
			}
			// 正常应答发两次（模拟多 socket 重复），再发一个"自己"的应答
			reply := encodeResponse(config.ProtocolVersion, probe.Nonce, testServer, nil)
			_, _ = responder.WriteToUDP(reply, raddr)
			_, _ = responder.WriteToUDP(reply, raddr)
			self := testServer
			self.InstanceID = selfID
			_, _ = responder.WriteToUDP(encodeResponse(config.ProtocolVersion, probe.Nonce, self, nil), raddr)
		}
	}()

	sender, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen sender: %v", err)
	}
	senders := []probeSender{{
		conn:    sender,
		targets: []*net.UDPAddr{responder.LocalAddr().(*net.UDPAddr)},
	}}
	servers, err := discoverOn(senders, 500*time.Millisecond, "", selfID)
	if err != nil {
		t.Fatalf("discoverOn: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want 1 (dedupe/self-filter): %+v", len(servers), servers)
	}
	got := servers[0]
	if got.InstanceID != testServer.InstanceID || got.Alias != testServer.Alias ||
		got.TCPPort != testServer.TCPPort || got.IP != "127.0.0.1" {
		t.Errorf("unexpected result: %+v", got)
	}
	if got.Addr() != "127.0.0.1:52346" {
		t.Errorf("Addr() = %s", got.Addr())
	}
}
