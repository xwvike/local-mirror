package network

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/zeebo/blake3"
)

// TestHashResumePrefix 续传前缀哈希必须与"直接哈希同样字节"一致，
// 且分片缺失、分片比 offset 短都要报错，而不是悄悄算出一个错的哈希
func TestHashResumePrefix(t *testing.T) {
	data := bytes.Repeat([]byte("local-mirror resume "), 4096)
	path := filepath.Join(t.TempDir(), "x.part")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	const offset = 30000
	got := blake3.New()
	if err := hashResumePrefix(got, path, offset); err != nil {
		t.Fatalf("hashResumePrefix: %v", err)
	}
	want := blake3.New()
	want.Write(data[:offset])
	if !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		t.Error("前缀哈希与直接哈希同样字节的结果不一致")
	}

	if err := hashResumePrefix(blake3.New(), filepath.Join(t.TempDir(), "missing.part"), 1); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("分片缺失应返回 ErrNotExist，得到 %v", err)
	}
	if err := hashResumePrefix(blake3.New(), path, uint64(len(data))+1); !errors.Is(err, io.EOF) {
		t.Errorf("offset 超出分片长度应报 EOF，得到 %v", err)
	}
}
