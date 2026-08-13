package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func reset() {
	mu.Lock()
	snap = Snapshot{}
	path = ""
	observeDir = ""
	observedWriters = nil
	enabled = false
	lifePath = ""
	lifeFlushedBytes = 0
	mu.Unlock()
}

// TestObserveGate 观测门：无心跳=未观测；TouchObserve=已观测；心跳陈旧则未观测且
// 被清理；ClearObserve 撤销
func TestObserveGate(t *testing.T) {
	reset()
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".local-mirror"), 0755)
	Init(root, "v1", "aa", "send · source", "listen", "inbound", false, time.Now().Unix())

	if observedNow() {
		t.Fatal("no heartbeat should read unobserved")
	}
	TouchObserve(root)
	if !observedNow() {
		t.Fatal("after TouchObserve should be observed")
	}
	// 回拨心跳 mtime 超出 observeWindow → 未观测且顺手清理
	hb := filepath.Join(root, ".local-mirror", "observe", strconv.Itoa(os.Getpid()))
	old := time.Now().Add(-2 * observeWindow)
	if err := os.Chtimes(hb, old, old); err != nil {
		t.Fatal(err)
	}
	if observedNow() {
		t.Fatal("stale heartbeat should read unobserved")
	}
	if _, err := os.Stat(hb); !os.IsNotExist(err) {
		t.Fatal("stale heartbeat should be pruned")
	}
	// 再投一次然后撤销
	TouchObserve(root)
	ClearObserve(root)
	if observedNow() {
		t.Fatal("after ClearObserve should be unobserved")
	}
}

// TestObservedWriter 注册的 observed-writer 会随 writeAll 一起触发
func TestObservedWriter(t *testing.T) {
	reset()
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".local-mirror"), 0755)
	Init(root, "v1", "aa", "send · source", "listen", "inbound", false, time.Now().Unix())
	called := 0
	RegisterObservedWriter(func() { called++ })
	writeAll()
	if called != 1 {
		t.Fatalf("observed writer should fire once per writeAll, got %d", called)
	}
}

// TestAwaitFresh status.json 在 since 之后被写 → 视为新鲜；无文件 → 超时
func TestAwaitFresh(t *testing.T) {
	reset()
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".local-mirror"), 0755)
	Init(root, "v1", "aa", "receive · sink", "dial", "peer", false, time.Now().Unix())

	if AwaitFresh(root, time.Now(), 150*time.Millisecond) {
		t.Fatal("no status.json should time out")
	}
	before := time.Now().Add(-time.Second)
	write()
	if !AwaitFresh(root, before, 500*time.Millisecond) {
		t.Fatal("a written status.json should count as fresh vs an earlier 'since'")
	}
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

// TestLifetimePersists 终身累计跨"重启"续算：Init→传输→Flush 落 stats.json，
// 再次 Init 同一根应载回累计值并在其上继续叠加；会话计数则重启归零，
// SinceUnix 保持首次起算点不变
func TestLifetimePersists(t *testing.T) {
	reset()
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".local-mirror"), 0755)

	first := time.Now().Add(-time.Hour).Unix()
	Init(root, "v1", "aa", "send · source", "listen", "inbound", false, first)
	if snap.LifetimeSinceUnix != first {
		t.Fatalf("first run since = %d, want %d", snap.LifetimeSinceUnix, first)
	}
	RecordFile("a.txt", 100)
	RecordFile("b.bin", 900)
	FlushLifetime()

	// 模拟重启：清空内存态，用一个更晚的 started 重新 Init 同一根（stats.json 仍在盘上）
	reset()
	Init(root, "v1", "aa", "send · source", "listen", "inbound", false, time.Now().Unix())
	if snap.LifetimeFiles != 2 || snap.LifetimeBytes != 1000 {
		t.Fatalf("lifetime not restored: %d files / %d bytes", snap.LifetimeFiles, snap.LifetimeBytes)
	}
	if snap.LifetimeSinceUnix != first {
		t.Fatalf("since should persist across restart: got %d, want %d", snap.LifetimeSinceUnix, first)
	}
	if snap.Files != 0 || snap.Bytes != 0 {
		t.Fatalf("session counters should reset on restart: %d/%d", snap.Files, snap.Bytes)
	}

	// 续算：再传一个，终身在恢复值上叠加，会话只计本轮
	RecordFile("c.dat", 500)
	if snap.LifetimeFiles != 3 || snap.LifetimeBytes != 1500 {
		t.Fatalf("lifetime should keep accumulating: %d files / %d bytes", snap.LifetimeFiles, snap.LifetimeBytes)
	}
	if snap.Files != 1 || snap.Bytes != 500 {
		t.Fatalf("session should count only this run: %d/%d", snap.Files, snap.Bytes)
	}
}

// TestLifetimeAutoFlushByBytes 落盘阈值以传输量为准而非文件数：累计不足阈值
// 不落盘（stats.json 不存在），跨过阈值即自动落盘。文件数无关——一个大文件
// 就能触发，上万个小文件也只在攒够字节时才落
func TestLifetimeAutoFlushByBytes(t *testing.T) {
	reset()
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".local-mirror"), 0755)
	Init(root, "v1", "aa", "send · source", "listen", "inbound", false, time.Now().Unix())
	statsPath := filepath.Join(root, ".local-mirror", "stats.json")

	// 不足 1 MiB：多个文件但字节没攒够 → 不自动落盘
	RecordFile("a", 200*1024)
	RecordFile("b", 300*1024)
	if _, err := os.Stat(statsPath); !os.IsNotExist(err) {
		t.Fatalf("under threshold should not auto-flush, but stats.json exists")
	}

	// 一个大文件一步跨过阈值 → 自动落盘，且落的是含它的累计值
	RecordFile("big", 2*1024*1024)
	data, err := os.ReadFile(statsPath)
	if err != nil {
		t.Fatalf("crossing threshold should auto-flush stats.json: %v", err)
	}
	var ls lifetimeState
	if err := json.Unmarshal(data, &ls); err != nil {
		t.Fatalf("stats.json unparsable: %v", err)
	}
	if ls.Files != 3 || ls.Bytes != 200*1024+300*1024+2*1024*1024 {
		t.Fatalf("flushed totals wrong: %d files / %d bytes", ls.Files, ls.Bytes)
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

// TestProgressAndRate 进行中传输字段落盘、完成后清空、速率非负
func TestProgressAndRate(t *testing.T) {
	reset()
	rateSamples = nil
	lastSampleAt = time.Time{}
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".local-mirror"), 0755)
	Init(root, "v1", "aa", "receive · sink", "dial", "peer", true, time.Now().Unix())

	// 进行中：current_* 应落盘
	RecordProgress("big.bin", 500, 1000)
	write()
	s, _ := Load(root)
	if s.CurrentFile != "big.bin" || s.CurrentDone != 500 || s.CurrentTotal != 1000 {
		t.Fatalf("in-flight fields wrong: %q %d/%d", s.CurrentFile, s.CurrentDone, s.CurrentTotal)
	}

	// 完成：current_* 清空，累计 +total
	RecordFile("big.bin", 1000)
	write()
	s, _ = Load(root)
	if s.CurrentFile != "" || s.CurrentDone != 0 {
		t.Fatalf("completed transfer should clear current: %q %d", s.CurrentFile, s.CurrentDone)
	}
	if s.Files != 1 || s.Bytes != 1000 {
		t.Fatalf("totals after complete: %d files / %d bytes", s.Files, s.Bytes)
	}
	if s.RateBps < 0 {
		t.Fatalf("rate must not be negative: %v", s.RateBps)
	}
}

// TestRateWindowDecays 停止上报后，窗口内取样耗尽 → 速率归 0
func TestRateWindowDecays(t *testing.T) {
	reset()
	rateSamples = nil
	// 人为塞两个都早于窗口的取样，computeRate 修剪后应剩 <2 → 0
	old := time.Now().Add(-2 * rateWindow)
	rateSamples = []rateSample{{old, 100}, {old.Add(time.Second), 200}}
	if r := computeRateLocked(time.Now()); r != 0 {
		t.Fatalf("stale samples should yield 0 rate, got %v", r)
	}
}

// TestResourceSampling 资源采样填入运行时数字（堆/goroutine 必非零）
func TestResourceSampling(t *testing.T) {
	reset()
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".local-mirror"), 0755)
	Init(root, "v1", "aa", "send · source", "listen", "inbound", false, time.Now().Unix())
	write()
	s, _ := Load(root)
	if s.HeapBytes == 0 {
		t.Fatal("heap bytes should be sampled non-zero")
	}
	if s.Goroutines <= 0 {
		t.Fatalf("goroutines should be positive, got %d", s.Goroutines)
	}
	if s.Schema != SchemaVersion {
		t.Fatalf("schema should be %d, got %d", SchemaVersion, s.Schema)
	}
}

// TestLooksLikeDaemon 只认二进制名 local-mirror 且无"读完即退"旗子的进程
func TestLooksLikeDaemon(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"/usr/local/bin/local-mirror", "--send", "-p", "/srv"}, true},
		{[]string{"local-mirror", "-m", "mirror", "-r", "host"}, true},
		{[]string{"/usr/local/bin/local-mirror", "--status", "-p", "/srv"}, false}, // 观测进程
		{[]string{"local-mirror", "--gen-key"}, false},
		{[]string{"local-mirror", "--version"}, false},
		{[]string{"/usr/bin/other-tool", "--send"}, false}, // 名字不对
		{nil, false},
	}
	for _, c := range cases {
		if got := looksLikeDaemon(c.args); got != c.want {
			t.Errorf("looksLikeDaemon(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}

// TestResolveRoot -p/--path 优先（相对挂 cwd），未指定则用 cwd
func TestResolveRoot(t *testing.T) {
	cases := []struct {
		args []string
		cwd  string
		want string
	}{
		{[]string{"local-mirror", "-p", "/srv/data"}, "/home/x", "/srv/data"},
		{[]string{"local-mirror", "--path", "/srv/data"}, "/home/x", "/srv/data"},
		{[]string{"local-mirror", "-p=/srv/data"}, "/home/x", "/srv/data"},
		{[]string{"local-mirror", "--path=/srv/data"}, "/home/x", "/srv/data"},
		{[]string{"local-mirror", "-p", "sub"}, "/home/x", "/home/x/sub"}, // 相对挂 cwd
		{[]string{"local-mirror", "--send"}, "/home/x", "/home/x"},        // 无 -p → cwd
		{[]string{"local-mirror", "-p", "sub"}, "", ""},                   // 相对但无 cwd → 无解
	}
	for _, c := range cases {
		if got := resolveRoot(c.args, c.cwd); got != c.want {
			t.Errorf("resolveRoot(%v, %q) = %q, want %q", c.args, c.cwd, got, c.want)
		}
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
