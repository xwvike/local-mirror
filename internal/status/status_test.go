package status

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func reset() {
	mu.Lock()
	snap = Snapshot{}
	path = ""
	enabled = false
	mu.Unlock()
}

// TestLoadMissing 无快照文件 → (nil, nil)，供 --status 报「无实例」
func TestLoadMissing(t *testing.T) {
	root := t.TempDir()
	s, err := Load(root)
	if err != nil || s != nil {
		t.Fatalf("missing status should be (nil, nil), got (%v, %v)", s, err)
	}
}

// TestInitWriteLoad identity 定型 + 写盘 + 读回一致
func TestInitWriteLoad(t *testing.T) {
	reset()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".local-mirror"), 0755); err != nil {
		t.Fatal(err)
	}
	Init(root, "v9.9.9", "deadbeef", "send · source", "dial", "vps:52345", true, time.Now().Unix())
	write()

	s, err := Load(root)
	if err != nil || s == nil {
		t.Fatalf("Load after write: (%v, %v)", s, err)
	}
	if s.Version != "v9.9.9" || s.Instance != "deadbeef" || s.Direction != "send · source" ||
		s.Transport != "dial" || s.Peer != "vps:52345" || !s.Encrypted {
		t.Fatalf("identity round-trip mismatch: %+v", s)
	}
	if s.Schema != SchemaVersion {
		t.Fatalf("schema %d, want %d", s.Schema, SchemaVersion)
	}
	if s.PID != os.Getpid() {
		t.Fatalf("pid %d, want %d", s.PID, os.Getpid())
	}
}

// TestCounters 文件/错误/连接计数累积正确
func TestCounters(t *testing.T) {
	reset()
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".local-mirror"), 0755)
	Init(root, "v1", "aa", "receive · sink", "dial", "peer", false, time.Now().Unix())

	RecordFile("a.txt", 100)
	RecordFile("b/c.bin", 900)
	RecordError()
	SessionUp("connected to peer")
	write()

	s, _ := Load(root)
	if s.Files != 2 || s.Bytes != 1000 {
		t.Fatalf("files/bytes = %d/%d, want 2/1000", s.Files, s.Bytes)
	}
	if s.LastFile != "b/c.bin" || s.LastSyncUnix == 0 {
		t.Fatalf("last file %q / sync %d not recorded", s.LastFile, s.LastSyncUnix)
	}
	if s.Errors != 1 {
		t.Fatalf("errors %d, want 1", s.Errors)
	}
	if !s.Connected || s.Peers != 1 || s.Detail != "connected to peer" {
		t.Fatalf("session up not reflected: connected=%v peers=%d detail=%q", s.Connected, s.Peers, s.Detail)
	}
}

// TestSessionBalance up/down 平衡后 Connected 归 false，Detail 清空
func TestSessionBalance(t *testing.T) {
	reset()
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".local-mirror"), 0755)
	Init(root, "v1", "aa", "send · source", "listen", "inbound", false, time.Now().Unix())

	SessionUp("serving A")
	SessionUp("serving B") // 源可扇出多个下游
	if snap.Peers != 2 || !snap.Connected {
		t.Fatalf("two peers: peers=%d connected=%v", snap.Peers, snap.Connected)
	}
	SessionDown()
	if !snap.Connected || snap.Peers != 1 {
		t.Fatalf("one down: still one peer expected, got peers=%d connected=%v", snap.Peers, snap.Connected)
	}
	SessionDown()
	if snap.Connected || snap.Peers != 0 || snap.Detail != "" {
		t.Fatalf("all down: peers=%d connected=%v detail=%q", snap.Peers, snap.Connected, snap.Detail)
	}
	// 多减不为负
	SessionDown()
	if snap.Peers != 0 {
		t.Fatalf("peers went negative: %d", snap.Peers)
	}
}

// TestStale 陈旧判据：新写不陈旧，人为回拨 updated_unix 则陈旧
func TestStale(t *testing.T) {
	reset()
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".local-mirror"), 0755)
	Init(root, "v1", "aa", "receive · sink", "dial", "peer", false, time.Now().Unix())
	write()
	s, _ := Load(root)
	if s.Stale() {
		t.Fatal("fresh snapshot should not be stale")
	}
	s.UpdatedUnix = time.Now().Add(-1 * time.Minute).Unix()
	if !s.Stale() {
		t.Fatalf("snapshot %s old should be stale", s.Age())
	}
}

// TestAtomicWriteNoTemp write 后不残留 .tmp（rename 原子替换）
func TestAtomicWriteNoTemp(t *testing.T) {
	reset()
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".local-mirror"), 0755)
	Init(root, "v1", "aa", "send · source", "listen", "inbound", false, time.Now().Unix())
	write()
	if _, err := os.Stat(filepath.Join(root, ".local-mirror", "status.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("temp file should not linger after atomic write")
	}
}
