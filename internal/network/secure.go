package network

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/flynn/noise"
	"github.com/zeebo/blake3"
)

// 传输加密：Noise Protocol Framework 的 NNpsk0 模式
// （X25519 + ChaCha20-Poly1305 + BLAKE2s）。
// 用户口令派生的预共享密钥（PSK）完成双向认证——口令不一致时
// 第一条握手消息即失败；临时密钥 ECDH 提供前向保密。
// 握手完成后所有字节经 secureConn 透明加解密，上层协议无感知。

const (
	// noiseMaxPlaintext 单帧明文上限：Noise 消息总长上限 65535，
	// 需为 AEAD tag 留出 16 字节
	noiseMaxPlaintext = 65535 - 16
	// noiseHandshakeTimeout 加密握手限时，对端异常时快速失败
	noiseHandshakeTimeout = 5 * time.Second
	// noiseMaxHandshakeFrame 握手帧的合理上限。
	// 明文对端发来的协议魔术字会被误读成超大帧长度，用它提前识别配置不一致
	noiseMaxHandshakeFrame = 1024
)

var noiseCipherSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)

// ErrSecureHandshake 加密握手失败的哨兵错误。
// 端口扫描时它比"端口拒连"更有定位价值（通常意味着口令配置不一致）
var ErrSecureHandshake = errors.New("加密握手失败")

// DerivePSK 从用户口令派生 32 字节预共享密钥。
// 加入固定前缀做域分离，避免口令在其他场景复用时产生相同密钥
func DerivePSK(secret string) []byte {
	sum := blake3.Sum256([]byte("local-mirror-noise-psk-v1:" + secret))
	return sum[:]
}

// secureConn 包装底层连接，Write 加密、Read 解密，其余方法
// （含 SetDeadline 系列）通过嵌入透传给底层连接
type secureConn struct {
	net.Conn
	enc     *noise.CipherState
	dec     *noise.CipherState
	readBuf bytes.Buffer // 已解密但尚未被消费的明文
}

// writeFrame 写入一个带 2 字节长度前缀的帧。
// 长度与数据一次性写出，避免半帧交错
func writeFrame(conn net.Conn, data []byte) error {
	frame := make([]byte, 2+len(data))
	binary.BigEndian.PutUint16(frame, uint16(len(data)))
	copy(frame[2:], data)
	_, err := conn.Write(frame)
	return err
}

func readFrame(conn net.Conn, maxLen int) ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(lenBuf[:]))
	if n > maxLen {
		return nil, fmt.Errorf("frame too large: %d bytes (对端可能未启用加密)", n)
	}
	frame := make([]byte, n)
	if _, err := io.ReadFull(conn, frame); err != nil {
		return nil, err
	}
	return frame, nil
}

func (s *secureConn) Read(p []byte) (int, error) {
	for s.readBuf.Len() == 0 {
		frame, err := readFrame(s.Conn, 65535)
		if err != nil {
			return 0, err
		}
		plain, err := s.dec.Decrypt(nil, nil, frame)
		if err != nil {
			return 0, fmt.Errorf("decrypt failed (密钥不匹配或数据被篡改): %w", err)
		}
		s.readBuf.Write(plain)
	}
	return s.readBuf.Read(p)
}

func (s *secureConn) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := min(len(p), noiseMaxPlaintext)
		ciphertext, err := s.enc.Encrypt(nil, nil, p[:n])
		if err != nil {
			return total, fmt.Errorf("encrypt failed: %w", err)
		}
		if err := writeFrame(s.Conn, ciphertext); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

// SecureConn 在已建立的 TCP 连接上执行 Noise NNpsk0 握手，
// 返回透明加解密的连接。双方必须使用相同口令，否则握手失败。
// initiator 为 true 表示客户端（主动发起方）
func SecureConn(conn net.Conn, secret string, initiator bool) (net.Conn, error) {
	conn.SetDeadline(time.Now().Add(noiseHandshakeTimeout))
	defer conn.SetDeadline(time.Time{})

	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:           noiseCipherSuite,
		Pattern:               noise.HandshakeNN,
		Initiator:             initiator,
		PresharedKey:          DerivePSK(secret),
		PresharedKeyPlacement: 0,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create noise handshake: %w", err)
	}

	if initiator {
		// -> psk, e
		msg, _, _, err := hs.WriteMessage(nil, nil)
		if err != nil {
			return nil, fmt.Errorf("noise handshake write: %w", err)
		}
		if err := writeFrame(conn, msg); err != nil {
			return nil, fmt.Errorf("noise handshake send: %w", err)
		}
		// <- e, ee
		reply, err := readFrame(conn, noiseMaxHandshakeFrame)
		if err != nil {
			return nil, fmt.Errorf("noise handshake recv: %w", err)
		}
		_, cs0, cs1, err := hs.ReadMessage(nil, reply)
		if err != nil {
			return nil, fmt.Errorf("noise handshake failed（两端口令是否一致？）: %w", err)
		}
		// cs0 固定用于 发起方→响应方 方向
		return &secureConn{Conn: conn, enc: cs0, dec: cs1}, nil
	}

	// <- psk, e
	first, err := readFrame(conn, noiseMaxHandshakeFrame)
	if err != nil {
		return nil, fmt.Errorf("noise handshake recv: %w", err)
	}
	if _, _, _, err := hs.ReadMessage(nil, first); err != nil {
		return nil, fmt.Errorf("noise handshake failed（对端口令不一致或未启用加密）: %w", err)
	}
	// -> e, ee
	msg, cs0, cs1, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("noise handshake write: %w", err)
	}
	if err := writeFrame(conn, msg); err != nil {
		return nil, fmt.Errorf("noise handshake send: %w", err)
	}
	return &secureConn{Conn: conn, enc: cs1, dec: cs0}, nil
}
