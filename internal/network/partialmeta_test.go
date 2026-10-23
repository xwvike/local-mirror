package network

import (
	"bytes"
	"local-mirror/config"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/zeebo/blake3"
)

// streamFile 在 conn 对端按 fs 发送 data；complete 为假时发完数据就断开，模拟传输中断
func streamFile(t *testing.T, conn net.Conn, fs fileSession, data []byte, complete bool) {
	t.Helper()
	go func() {
		defer conn.Close()
		for off := 0; off < len(data); off += 32 << 10 {
			chunk := data[off:min(off+32<<10, len(data))]
			msg := FileDataMessage{SessionID: fs.ID, DataLength: uint32(len(chunk)), Data: chunk}
			if sendMessage(conn, MsgTypeFileData, encodeFileData(msg)) != nil {
				return
			}
		}
		if complete {
			sendMessage(conn, MsgTypeFileComplete, encodeFileComplete(FileCompleteMessage{SessionID: fs.ID, FileHash: blake3.Sum256(data)}))
		}
	}()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// 小文件不记分片元数据（中断后不续传、下次从头覆盖），大文件照旧记下以便续传
func TestPartialMetaOnlyForLargeFiles(t *testing.T) {
	config.StartPath = t.TempDir()
	if err := os.MkdirAll(filepath.Join(config.StartPath, ".local-mirror", "partial"), 0o755); err != nil {
		t.Fatal(err)
	}
	small := bytes.Repeat([]byte("s"), 40<<10)
	large := bytes.Repeat([]byte("l"), resumeMinSize)

	for _, c := range []struct {
		name     string
		data     []byte
		wantMeta bool
	}{{"small.bin", small, false}, {"large.bin", large, true}} {
		fs := fileSession{Path: c.name, Size: uint64(len(c.data)), ID: [16]byte{1}, StartHash: "h"}
		a, b := net.Pipe()
		streamFile(t, a, fs, c.data, false)
		if _, err := receiveFile(b, fs); err == nil {
			t.Fatalf("%s: 中断的传输应报错", c.name)
		}
		partialPath, metaPath := partialPaths(c.name)
		if !exists(partialPath) {
			t.Errorf("%s: 中断后应留下分片", c.name)
		}
		if exists(metaPath) != c.wantMeta {
			t.Errorf("%s: 元数据存在 = %v，想要 %v", c.name, exists(metaPath), c.wantMeta)
		}
	}

	// 小文件再收一次：残留分片被覆盖，收完分片与元数据都不留
	fs := fileSession{Path: "small.bin", Size: uint64(len(small)), ID: [16]byte{2}, StartHash: "h"}
	a, b := net.Pipe()
	streamFile(t, a, fs, small, true)
	if _, err := receiveFile(b, fs); err != nil {
		t.Fatalf("覆盖残留分片后接收: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(config.StartPath, "small.bin"))
	if err != nil || !bytes.Equal(got, small) {
		t.Fatalf("落盘内容不符: %v", err)
	}
	if partialPath, metaPath := partialPaths("small.bin"); exists(partialPath) || exists(metaPath) {
		t.Error("收完后分片或元数据仍在")
	}
}
