package network

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/internal/tree"

	"github.com/zeebo/blake3"
)

// latencyConn 给写出的数据加单程时延而不限带宽：Write 立即返回，数据 delay 之后才交给
// 底层连接，两端各包一层即得 RTT = 2*delay。写超时不往下传（数据由 pump 稍后写出）
type latencyConn struct {
	net.Conn
	delay time.Duration
	queue chan timedChunk
	done  chan struct{}
	once  sync.Once
}

type timedChunk struct {
	at   time.Time
	data []byte
}

func newLatencyConn(c net.Conn, delay time.Duration) *latencyConn {
	l := &latencyConn{Conn: c, delay: delay, queue: make(chan timedChunk, 1<<14), done: make(chan struct{})}
	go l.pump()
	return l
}

func (l *latencyConn) pump() {
	for {
		select {
		case ch := <-l.queue:
			time.Sleep(time.Until(ch.at))
			if _, err := l.Conn.Write(ch.data); err != nil {
				return
			}
		case <-l.done:
			return
		}
	}
}

func (l *latencyConn) Write(p []byte) (int, error) {
	chunk := timedChunk{at: time.Now().Add(l.delay), data: append([]byte(nil), p...)}
	select {
	case l.queue <- chunk:
		return len(p), nil
	case <-l.done:
		return 0, net.ErrClosed
	}
}

func (l *latencyConn) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Conn.Close()
}

func (l *latencyConn) SetWriteDeadline(time.Time) error { return nil }
func (l *latencyConn) SetDeadline(t time.Time) error    { return l.Conn.SetReadDeadline(t) }

// setupFetchRoot 建同步根并写入文件、建树。源端与汇端同进程共用 config.StartPath 与树库，
// 汇端下载的文件落回同一路径（内容不变），足以检验协议往返、校验与落盘流程
func setupFetchRoot(t *testing.T, files map[string][]byte) string {
	t.Helper()
	root := t.TempDir()
	config.StartPath = root
	savedIgnore := config.IgnoreFileList
	config.IgnoreFileList = []string{".local-mirror"}
	bufSize := uint64(32 << 10)
	savedBuf := config.FileBufferSize
	config.FileBufferSize = &bufSize
	savedSnap := config.SnapshotOverwrites
	config.SnapshotOverwrites = false
	t.Cleanup(func() {
		config.IgnoreFileList = savedIgnore
		config.FileBufferSize = savedBuf
		config.SnapshotOverwrites = savedSnap
	})
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tree.InitDB()
	t.Cleanup(func() { tree.DB.Close() })
	if err := tree.BuildFileTree(root); err != nil {
		t.Fatalf("BuildFileTree: %v", err)
	}
	return root
}

// connectFetch 在 net.Pipe 上起源端消息循环并完成握手。delay > 0 时两端各加单程时延；
// delay == 0 时是完全同步的 net.Pipe（无任何缓冲：协议若要求两端同时写就会死锁）
func connectFetch(t *testing.T, delay time.Duration) *FileClient {
	t.Helper()
	var cli, srv net.Conn
	cli, srv = net.Pipe()
	if delay > 0 {
		cli, srv = newLatencyConn(cli, delay), newLatencyConn(srv, delay)
	}
	s := &fileServer{}
	served := make(chan struct{})
	go func() {
		defer close(served)
		s.serveConn(srv, nil)
	}()
	fc := NewFileClientFromConn(cli)
	t.Cleanup(func() {
		fc.ConnectionClose()
		<-served
	})
	// 同进程两端实例 ID 相同，Handshake 的自连接防护会拒绝；这里走同一条握手报文、
	// 照 Handshake 记下对端信息
	if err := sendMessage(cli, MsgTypeHandshake, encodeHandshake(localHandshake())); err != nil {
		t.Fatalf("send handshake: %v", err)
	}
	typ, body, err := receiveMessage(cli)
	if err != nil || typ != MsgTypeHandshake {
		t.Fatalf("handshake response: type %d, %v", typ, err)
	}
	resp, err := decodeHandshake(body)
	if err != nil {
		t.Fatal(err)
	}
	fc.realityVersion, fc.realityID, fc.peerFeatures, fc.State = resp.Version, resp.UUID, resp.FeatureBits, Online
	return fc
}

type fetchResult struct {
	hash string
	err  error
}

func fetchAll(t *testing.T, fc *FileClient, paths ...string) (map[string]fetchResult, []string, error) {
	t.Helper()
	files := make([]FetchFile, len(paths))
	for i, p := range paths {
		files[i] = FetchFile{Path: p, Perm: 0o644}
	}
	got := make(map[string]fetchResult)
	var order []string
	err := fc.FetchFiles(files, func(i int, hash string, err error) {
		if _, dup := got[paths[i]]; dup {
			t.Errorf("%s reported twice", paths[i])
		}
		got[paths[i]] = fetchResult{hash, err}
		order = append(order, paths[i])
	})
	return got, order, err
}

func blake3Hex(b []byte) string {
	return fmt.Sprintf("%x", blake3.Sum256(b))
}

// N 个小文件逐个请求要付 N 个往返；批量下载只付一个往返外加传输时间
func TestFetchFilesOneRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("measures round trips with injected latency")
	}
	const n = 40
	const delay = 25 * time.Millisecond // 单程，RTT = 50ms
	const rtt = 2 * delay
	files := make(map[string][]byte)
	var paths []string
	for i := range n {
		p := filepath.Join("d", fmt.Sprintf("f%02d.txt", i))
		files[p] = []byte(strings.Repeat(fmt.Sprintf("file %d ", i), 200))
		paths = append(paths, p)
	}
	setupFetchRoot(t, files)

	batch := func(delay time.Duration) time.Duration {
		fc := connectFetch(t, delay)
		start := time.Now()
		got, _, err := fetchAll(t, fc, paths...)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("FetchFiles: %v", err)
		}
		for _, p := range paths {
			if r := got[p]; r.err != nil || r.hash != blake3Hex(files[p]) {
				t.Fatalf("%s: hash %q err %v", p, r.hash, r.err)
			}
		}
		return elapsed
	}

	// 无时延下的批量耗时 = 纯传输与落盘成本
	local := batch(0)
	withLatency := batch(delay)

	fc := connectFetch(t, delay)
	start := time.Now()
	for _, p := range paths {
		if _, err := fc.DownloadFile(p, 0o644); err != nil {
			t.Fatalf("DownloadFile %s: %v", p, err)
		}
	}
	serial := time.Since(start)

	t.Logf("%d files, RTT %v: batched %v (no latency %v), one by one %v", n, rtt, withLatency, local, serial)
	if serial < n*rtt {
		t.Fatalf("one-by-one download took %v, expected at least %d round trips (%v): latency injection is not working", serial, n, n*rtt)
	}
	if limit := 2*rtt + 2*local; withLatency > limit {
		t.Fatalf("batched download took %v, want about one round trip plus transfer time (<= %v)", withLatency, limit)
	}
}

// 单项失败不影响其余项，结果与请求逐项对齐，流结束后连接照常可用。
// 跑在完全同步的 net.Pipe 上：源端推送期间汇端只读，任何一处需要两端同时写都会死锁
func TestFetchFilesPerItemFailures(t *testing.T) {
	files := map[string][]byte{"a.txt": []byte("alpha"), "b.txt": []byte("bravo"), "c.txt": []byte("charlie"), "gone.txt": []byte("x")}
	root := setupFetchRoot(t, files)
	// gone.txt 登记在树里、磁盘上已删除：源端给不出
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	fc := connectFetch(t, 0)

	outside := filepath.Join("..", "escape.txt")
	got, order, err := fetchAll(t, fc, "a.txt", "gone.txt", "missing.txt", outside, "b.txt")
	if err != nil {
		t.Fatalf("FetchFiles: %v", err)
	}
	// 越界路径本地就拒，不进请求；其余按请求顺序
	if want := []string{outside, "a.txt", "gone.txt", "missing.txt", "b.txt"}; !reflect.DeepEqual(order, want) {
		t.Errorf("callback order %v, want %v", order, want)
	}
	for _, p := range []string{"a.txt", "b.txt"} {
		if r := got[p]; r.err != nil || r.hash != blake3Hex(files[p]) {
			t.Errorf("%s: hash %q err %v", p, r.hash, r.err)
		}
	}
	for _, p := range []string{"gone.txt", "missing.txt"} {
		var re *RealityError
		if r := got[p]; !errors.As(r.err, &re) || re.Code != ErrCodeNotFound {
			t.Errorf("%s: want a not-found reality error, got %v", p, r.err)
		}
	}
	if r := got[outside]; r.err == nil || errors.Is(r.err, appError.ErrConnection) {
		t.Errorf("out-of-root path: want a local refusal, got %v", r.err)
	}

	// 同一连接继续可用
	got, _, err = fetchAll(t, fc, "c.txt")
	if err != nil || got["c.txt"].err != nil || got["c.txt"].hash != blake3Hex(files["c.txt"]) {
		t.Fatalf("follow-up fetch: %+v, %v", got, err)
	}
	if _, err := fc.GetRealityTree("."); err != nil {
		t.Fatalf("follow-up tree request: %v", err)
	}
}

// writePartial 伪造一次中断下载留下的分片与元数据
func writePartial(t *testing.T, path string, prefix []byte, meta partialMeta) {
	t.Helper()
	partialPath, metaPath := partialPaths(path)
	if err := os.MkdirAll(filepath.Dir(partialPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partialPath, prefix, 0o600); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(meta)
	if err := os.WriteFile(metaPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func partialGone(path string) bool {
	partialPath, metaPath := partialPaths(path)
	_, perr := os.Stat(partialPath)
	_, merr := os.Stat(metaPath)
	return os.IsNotExist(perr) && os.IsNotExist(merr)
}

// 分片续传语义与单文件请求一致：指纹相符从分片末尾续传；指纹不符（源文件中途变过）
// 丢弃分片、排空该会话，批内后续文件照常接收
func TestFetchFilesResume(t *testing.T) {
	big := []byte(strings.Repeat("0123456789abcdef", 8<<10)) // 128 KB，多个数据块
	files := map[string][]byte{"good.bin": big, "corrupt.bin": big, "stale.bin": big, "after.txt": []byte("after")}
	setupFetchRoot(t, files)
	fc := connectFetch(t, 0)
	full := partialMeta{Hash: blake3Hex(big), Size: uint64(len(big))}

	// 正确前缀：续传后整文件哈希相符
	writePartial(t, "good.bin", big[:50000], full)
	// 前缀被篡改但指纹相符：只有真从偏移处续传，才会因整文件哈希不符而失败
	tampered := append([]byte(nil), big[:50000]...)
	tampered[10] ^= 0xff
	writePartial(t, "corrupt.bin", tampered, full)
	// 指纹过期：源文件在中断期间变过
	writePartial(t, "stale.bin", big[:50000], partialMeta{Hash: blake3Hex([]byte("old")), Size: uint64(len(big))})

	got, _, err := fetchAll(t, fc, "good.bin", "corrupt.bin", "stale.bin", "after.txt")
	if err != nil {
		t.Fatalf("FetchFiles: %v", err)
	}
	if r := got["good.bin"]; r.err != nil || r.hash != full.Hash {
		t.Errorf("resumed file: hash %q err %v", r.hash, r.err)
	}
	if r := got["corrupt.bin"]; r.err == nil || !strings.Contains(r.err.Error(), "hash mismatch") {
		t.Errorf("tampered prefix: want a hash mismatch (proves the transfer resumed from the offset), got %v", r.err)
	}
	if r := got["stale.bin"]; r.err == nil || !strings.Contains(r.err.Error(), "stale") || errors.Is(r.err, appError.ErrConnection) {
		t.Errorf("stale partial: want a non-connection stale error, got %v", r.err)
	}
	if r := got["after.txt"]; r.err != nil || r.hash != blake3Hex(files["after.txt"]) {
		t.Errorf("file after a drained session: hash %q err %v", r.hash, r.err)
	}
	for _, p := range []string{"good.bin", "corrupt.bin", "stale.bin"} {
		if !partialGone(p) {
			t.Errorf("%s: partial should be gone", p)
		}
	}
	// 失败的两个下一次从头取，成功
	got, _, err = fetchAll(t, fc, "corrupt.bin", "stale.bin")
	if err != nil || got["corrupt.bin"].err != nil || got["stale.bin"].err != nil {
		t.Fatalf("retry from scratch: %+v, %v", got, err)
	}
}

// 中途断连：出错的那一项带着连接错误回调，其后的项不回调，FetchFiles 返回连接错误
func TestFetchFilesConnectionLoss(t *testing.T) {
	files := map[string][]byte{"a.txt": []byte("alpha"), "b.txt": []byte("bravo")}
	setupFetchRoot(t, files)
	cli, srv := net.Pipe()
	fc := NewFileClientFromConn(cli)
	defer fc.ConnectionClose()
	// 源端读完请求就断开
	go func() {
		receiveMessage(srv)
		srv.Close()
	}()
	got, order, err := fetchAll(t, fc, "a.txt", "b.txt")
	if !errors.Is(err, appError.ErrConnection) {
		t.Fatalf("want a connection error, got %v", err)
	}
	if !reflect.DeepEqual(order, []string{"a.txt"}) || !errors.Is(got["a.txt"].err, appError.ErrConnection) {
		t.Fatalf("want only a.txt reported with the connection error, got %v %+v", order, got)
	}
}

// 能力位门控：只有两端都置位才批量下载；任一端是旧版时汇端仍逐个请求，且照常可用
func TestFetchBatchFeatureGating(t *testing.T) {
	files := map[string][]byte{"a.txt": []byte("alpha")}
	setupFetchRoot(t, files)

	check := func(name string, sink, source uint64, want bool) {
		t.Run(name, func(t *testing.T) {
			savedSink, savedSource := sinkFeatures, sourceFeatures
			sinkFeatures, sourceFeatures = sink, source
			defer func() { sinkFeatures, sourceFeatures = savedSink, savedSource }()
			fc := connectFetch(t, 0)
			if got := fc.SupportsFetchBatch(); got != want {
				t.Fatalf("SupportsFetchBatch = %v, want %v", got, want)
			}
			if hash, err := fc.DownloadFile("a.txt", 0o644); err != nil || hash != blake3Hex(files["a.txt"]) {
				t.Fatalf("one-by-one download: %q, %v", hash, err)
			}
		})
	}
	const v2102 = FeatureBulkPush | FeatureSourceIgnores // v2.10.2 申报的能力位
	all := v2102 | FeatureFetchBatch
	check("both new", all, all, true)
	check("old source", all, v2102, false)
	check("old sink", v2102, all, false)
}

func TestFetchRequestRoundTrip(t *testing.T) {
	req := FetchRequestMessage{Items: []FetchItem{
		{Path: filepath.Join("a", "b.txt"), Offset: 0},
		{Path: "c", Offset: 1 << 40},
	}}
	got, err := decodeFetchRequest(encodeFetchRequest(req))
	if err != nil || !reflect.DeepEqual(got, req) {
		t.Errorf("round trip: got %+v, %v", got, err)
	}
	empty, err := decodeFetchRequest(encodeFetchRequest(FetchRequestMessage{}))
	if err != nil || len(empty.Items) != 0 {
		t.Errorf("empty round trip: got %+v, %v", empty, err)
	}
}

// 伪造的超大条目计数必须在分配前被拒绝
func TestDecodeFetchRequestRejectsForgedCount(t *testing.T) {
	body := binary.BigEndian.AppendUint32(nil, 1<<30)
	body = append(body, make([]byte, 64)...)
	if _, err := decodeFetchRequest(body); err == nil {
		t.Error("a forged item count was accepted")
	}
}
