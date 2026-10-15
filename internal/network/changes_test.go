package network

import (
	"net"
	"slices"
	"testing"
	"time"

	"local-mirror/config"
	"local-mirror/internal/tree"
)

// pollChanges 走一次真实的变更长轮询：经 net.Pipe 发请求、由 handler 应答
func pollChanges(t *testing.T, s *fileServer, start int64) RecentChangeResponseMessage {
	t.Helper()
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()
	c := &client{Conn: srv, Connected: true, Addr: "pipe"}
	errc := make(chan error, 1)
	go func() {
		_, body, err := receiveMessage(srv)
		if err != nil {
			errc <- err
			return
		}
		errc <- s.handleRecentChangeRequest(c, body)
	}()
	req := encodeRecentChangeRequest(RecentChangeRequestMessage{ClientID: 1, StartTime: start})
	if err := sendMessage(cli, MsgTypeRecentChangeRequest, req); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(LongPollHold + 5*time.Second))
	typ, body, err := receiveMessage(cli)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if typ != MsgTypeRecentChangeResponse {
		t.Fatalf("unexpected message type %d", typ)
	}
	if err := <-errc; err != nil {
		t.Fatalf("handler: %v", err)
	}
	resp, err := decodeRecentChangeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestRecentChangesDeliveredOnce 每条变更记录只交付一次，且之后写入的不会漏。
// 修复前覆盖点给 now 且交付到 now（含）：汇端处理完立刻按覆盖点再查，同一秒里
// 写入的记录被反复交付——实测一次新建文件引发 5000+ 轮空转，每轮一次整目录清单
func TestRecentChangesDeliveredOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on the change-log debounce and settle window")
	}
	root := t.TempDir()
	config.StartPath = root
	tree.InitDB()
	defer tree.DB.Close()
	s := &fileServer{}

	tree.AddRecentChangedDir("d")
	first := pollChanges(t, s, 0)
	if !slices.Contains(first.Changes, "d") {
		t.Fatalf("first poll should deliver d, got %v", first.Changes)
	}

	// 紧接着按覆盖点再查：d 不得再次出现；稍后写入的 e 必须交付
	time.AfterFunc(200*time.Millisecond, func() { tree.AddRecentChangedDir("e") })
	second := pollChanges(t, s, first.CoveredUntil)
	if slices.Contains(second.Changes, "d") {
		t.Fatalf("d was delivered again after the cursor passed it: %v", second.Changes)
	}
	if !slices.Contains(second.Changes, "e") {
		t.Fatalf("a change written after the first poll was missed: %v", second.Changes)
	}
}
