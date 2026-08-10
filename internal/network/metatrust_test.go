package network

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"local-mirror/internal/tree"
)

// TestTrustedServeHash 验证 5.2 的 serve 起始哈希 meta-trust 判定：磁盘 size+mtime 与树节点
// 一致且哈希可解时复用树哈希（ok=true），任一不符则回退（ok=false，调用方全量重算）。
func TestTrustedServeHash(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(p, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	validHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" // 64 hex = 32B
	base := &tree.Node{Size: uint64(fi.Size()), ModTime: fi.ModTime(), Hash: validHex}

	// 命中：size+mtime 一致、hash 可解 → ok，返回解码后的 32 字节
	if h, ok := trustedServeHash(base, fi); !ok {
		t.Error("size+mtime 一致且 hash 合法时应命中 meta-trust")
	} else {
		want, _ := hex.DecodeString(validHex)
		if hex.EncodeToString(h[:]) != validHex {
			t.Errorf("返回哈希应等于解码后的 node.Hash，得 %x 期望 %s", h, validHex)
		}
		_ = want
	}

	// size 不符 → 回退
	if _, ok := trustedServeHash(&tree.Node{Size: base.Size + 1, ModTime: fi.ModTime(), Hash: validHex}, fi); ok {
		t.Error("size 不符应回退")
	}
	// mtime 不符 → 回退
	if _, ok := trustedServeHash(&tree.Node{Size: base.Size, ModTime: fi.ModTime().Add(1), Hash: validHex}, fi); ok {
		t.Error("mtime 不符应回退")
	}
	// hash 为空 → 回退
	if _, ok := trustedServeHash(&tree.Node{Size: base.Size, ModTime: fi.ModTime(), Hash: ""}, fi); ok {
		t.Error("空哈希应回退")
	}
	// hash 非法（非 hex / 长度不对）→ 回退
	if _, ok := trustedServeHash(&tree.Node{Size: base.Size, ModTime: fi.ModTime(), Hash: "not-hex-zz"}, fi); ok {
		t.Error("非法哈希应回退")
	}
	if _, ok := trustedServeHash(&tree.Node{Size: base.Size, ModTime: fi.ModTime(), Hash: "abcd"}, fi); ok {
		t.Error("长度不足 32B 的哈希应回退")
	}
	// nil 节点 → 回退
	if _, ok := trustedServeHash(nil, fi); ok {
		t.Error("nil 节点应回退")
	}
}
