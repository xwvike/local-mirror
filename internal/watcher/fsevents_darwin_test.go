//go:build darwin

package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// newTestFSEvents 起一个监视 root 的 FSEvents 监视器并等它就绪：写一个探针文件，
// 收到它的事件才算流已开始投递，避免断言撞上启动窗口
func newTestFSEvents(t *testing.T, root string, dirs ...string) *fseventsWatcher {
	t.Helper()
	fw, err := newNativeWatcher(root)
	if err != nil {
		t.Fatal(err)
	}
	w := fw.(*fseventsWatcher)
	t.Cleanup(func() { w.Close() })
	for _, d := range append([]string{root}, dirs...) {
		if err := w.Add(d); err != nil {
			t.Fatal(err)
		}
	}
	probe := filepath.Join(root, ".probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, probe, fsnotify.Create)
	return w
}

// waitEvent 等到 name 上出现 op，其间的其他事件忽略
func waitEvent(t *testing.T, w *fseventsWatcher, name string, op fsnotify.Op) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-w.Events():
			if e.Name == name && e.Has(op) {
				return
			}
		case err := <-w.Errors():
			t.Fatalf("watcher error: %v", err)
		case <-deadline:
			t.Fatalf("no %v event for %s", op, name)
		}
	}
}

// drain 收集 d 时间内的全部事件
func drain(w *fseventsWatcher, d time.Duration) []fsnotify.Event {
	var out []fsnotify.Event
	deadline := time.After(d)
	for {
		select {
		case e := <-w.Events():
			out = append(out, e)
		case <-deadline:
			return out
		}
	}
}

func TestFSEventsBasicOps(t *testing.T) {
	// t.TempDir 在 /var/folders 下，/var 是指向 /private/var 的符号链接：
	// FSEvents 报告真实路径，这里顺带验证翻译回调用方给的前缀
	root := t.TempDir()
	a := filepath.Join(root, "a")
	if err := os.Mkdir(a, 0o755); err != nil {
		t.Fatal(err)
	}
	w := newTestFSEvents(t, root, a)

	f := filepath.Join(a, "f")
	if err := os.WriteFile(f, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, f, fsnotify.Create)

	if err := os.WriteFile(f, []byte("22"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 新建标志可能残留在之后的修改事件上，Create 与 Write 对下游等价（都会重哈希）
	waitEvent(t, w, f, fsnotify.Create|fsnotify.Write)

	if err := os.Chmod(f, 0o600); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, f, fsnotify.Create|fsnotify.Write|fsnotify.Chmod)

	g := filepath.Join(a, "g")
	if err := os.Rename(f, g); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, f, fsnotify.Remove)
	waitEvent(t, w, g, fsnotify.Create)

	if err := os.Remove(g); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, g, fsnotify.Remove)

	sub := filepath.Join(a, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, sub, fsnotify.Create)
}

// TestFSEventsNonRecursive 语义与 fsnotify 一致：只报告被监视目录的直接子项。
// 两级监控靠这一点决定哪些目录实时监听，同步根里被忽略的目录也靠它挡在外面
func TestFSEventsNonRecursive(t *testing.T) {
	root := t.TempDir()
	watched := filepath.Join(root, "watched")
	ignored := filepath.Join(root, "ignored")
	for _, d := range []string{watched, ignored} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w := newTestFSEvents(t, root, watched)

	if err := os.WriteFile(filepath.Join(ignored, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(watched, "marker")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, marker, fsnotify.Create)
	var leaked []string
	for _, e := range drain(w, 300*time.Millisecond) {
		if strings.HasPrefix(e.Name, ignored+"/") {
			leaked = append(leaked, e.Name)
		}
	}
	if len(leaked) > 0 {
		t.Errorf("events from an unwatched directory leaked: %v", leaked)
	}

	// Remove 之后不再报告
	if err := w.Remove(watched); err != nil {
		t.Fatal(err)
	}
	if err := w.Remove(watched); err == nil {
		t.Error("removing an unwatched directory should fail like fsnotify")
	}
	after := filepath.Join(watched, "after")
	if err := os.WriteFile(after, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(root, "probe2")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, probe, fsnotify.Create)
	for _, e := range drain(w, 300*time.Millisecond) {
		if e.Name == after {
			t.Errorf("event for %s after its directory was removed from the watch set", after)
		}
	}
}

// TestFSEventsAtomicReplace 写临时文件后 rename 覆盖：目标路径必须报成存在
// （Create），不能因为旧 inode 被替换掉而报 Remove
func TestFSEventsAtomicReplace(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := newTestFSEvents(t, root)

	tmp := filepath.Join(root, ".target.tmp")
	if err := os.WriteFile(tmp, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, target); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, target, fsnotify.Create)
	for _, e := range drain(w, 300*time.Millisecond) {
		if e.Name == target && e.Has(fsnotify.Remove) {
			t.Errorf("atomic replace reported as removal: %v", e)
		}
	}
}

// TestFSEventsDeletedDirLeavesWatchSet 被监视的目录删掉后随之移出集合（fsnotify 同样如此）
func TestFSEventsDeletedDirLeavesWatchSet(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "d")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	w := newTestFSEvents(t, root, d)
	if err := os.Remove(d); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, d, fsnotify.Remove)
	w.mu.Lock()
	_, still := w.watched[d]
	w.mu.Unlock()
	if still {
		t.Error("deleted directory is still in the watch set")
	}
}

func TestFSEventsCloseUnblocks(t *testing.T) {
	root := t.TempDir()
	fw, err := newNativeWatcher(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := fw.Add(root); err != nil {
		t.Fatal(err)
	}
	// 不消费事件：事件 channel 塞满后，回调与翻译 goroutine 都阻塞在投递上，
	// Close 仍须返回
	for i := range 3000 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	done := make(chan struct{})
	go func() { fw.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung")
	}
	if _, ok := <-fw.Events(); ok {
		// 缓冲里可能还有事件，读空后必须是关闭的
		for range fw.Events() {
		}
	}
	if err := fw.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestFSEventsFromReal(t *testing.T) {
	w := &fseventsWatcher{root: "/Users/me/Sync", realRoot: "/Users/me/Sync"}
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/Users/me/Sync", "/Users/me/Sync", true},
		{"/Users/me/Sync/a/b", "/Users/me/Sync/a/b", true},
		{"/users/ME/sync/a", "/Users/me/Sync/a", true}, // APFS 大小写不敏感
		{"/Users/me/SyncOther/a", "", false},
		{"/Users/me", "", false},
	}
	for _, c := range cases {
		got, ok := w.fromReal(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("fromReal(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
