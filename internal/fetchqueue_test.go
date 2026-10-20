package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/internal/network"
	"local-mirror/internal/tree"
)

// fakeFetcher 按脚本应答批量下载：results 里没有的路径算成功；breakAt 处出连接错误，
// 照 FetchFiles 的约定先随该项回调、其后的项不回调
type fakeFetcher struct {
	results map[string]error
	breakAt string
	calls   [][]string
	closed  int
}

func (f *fakeFetcher) FetchFiles(files []network.FetchFile, done func(int, string, error)) error {
	var paths []string
	for _, ff := range files {
		paths = append(paths, ff.Path)
	}
	f.calls = append(f.calls, paths)
	for i, ff := range files {
		if ff.Path == f.breakAt {
			err := fmt.Errorf("%w: peer went away", appError.ErrConnection)
			done(i, "", err)
			return err
		}
		if err := f.results[ff.Path]; err != nil {
			done(i, "", err)
			continue
		}
		done(i, "hash-"+ff.Path, nil)
	}
	return nil
}

func (f *fakeFetcher) ConnectionClose() { f.closed++ }

func setupFetchQueueRoot(t *testing.T) {
	t.Helper()
	config.StartPath = t.TempDir()
	tree.InitDB()
	t.Cleanup(func() { tree.DB.Close() })
}

func queued(paths ...string) fileQueue {
	var q fileQueue
	for _, p := range paths {
		q = append(q, DiffResult{Path: p, Name: filepath.Base(p), Action: "create", Hash: "h", Size: 1})
	}
	return q
}

type handled struct {
	path string
	err  error
}

func recordHandle(out *[]handled, abortOnConn bool) func(DiffResult, error) error {
	return func(v DiffResult, err error) error {
		*out = append(*out, handled{v.Path, err})
		if abortOnConn && errors.Is(err, appError.ErrConnection) {
			return err
		}
		return nil
	}
}

// 排队的文件按 fetchBatchMax 分批请求，每个文件的结果按序交给单项处理：
// 源端给不出的照常报错、权限拒绝按上游不可读跳过，都不影响其余文件
func TestFetchQueuedPerItemResults(t *testing.T) {
	setupFetchQueueRoot(t)
	var paths []string
	for i := range fetchBatchMax + 10 {
		paths = append(paths, fmt.Sprintf("f%03d", i))
	}
	notFound := &network.RealityError{Code: network.ErrCodeNotFound, Path: "f001"}
	f := &fakeFetcher{results: map[string]error{
		"f001": notFound,
		"f002": &network.RealityError{Code: network.ErrCodePermissionDenied, Path: "f002"},
	}}
	var got []handled
	var b tree.Batch
	if err := fetchQueued(f, queued(paths...), &b, recordHandle(&got, true)); err != nil {
		t.Fatalf("fetchQueued: %v", err)
	}
	if len(f.calls) != 2 || len(f.calls[0]) != fetchBatchMax || len(f.calls[1]) != 10 {
		t.Fatalf("want batches of %d and 10, got %d batches", fetchBatchMax, len(f.calls))
	}
	if len(got) != len(paths) {
		t.Fatalf("handled %d items, want %d", len(got), len(paths))
	}
	for i, h := range got {
		if h.path != paths[i] {
			t.Fatalf("item %d handled as %s, want %s (order must follow the queue)", i, h.path, paths[i])
		}
		switch h.path {
		case "f001":
			if !errors.Is(h.err, notFound) {
				t.Errorf("f001: want the not-found error, got %v", h.err)
			}
		default: // f002 权限拒绝 → 视为已处理
			if h.err != nil {
				t.Errorf("%s: unexpected error %v", h.path, h.err)
			}
		}
	}
	if f.closed != 0 {
		t.Errorf("connection closed %d times without a connection error", f.closed)
	}
}

// 连接错误归到出错的那一个文件（diffscan 据此计数、拉黑），其后的文件留给重连后的重试
func TestFetchQueuedConnectionErrorAttribution(t *testing.T) {
	setupFetchQueueRoot(t)
	f := &fakeFetcher{breakAt: "c"}
	var got []handled
	var b tree.Batch
	err := fetchQueued(f, queued("a", "b", "c", "d", "e"), &b, recordHandle(&got, true))
	if !errors.Is(err, appError.ErrConnection) {
		t.Fatalf("want a connection error, got %v", err)
	}
	var order []string
	for _, h := range got {
		order = append(order, h.path)
	}
	if !reflect.DeepEqual(order, []string{"a", "b", "c"}) || !errors.Is(got[2].err, appError.ErrConnection) {
		t.Fatalf("handled %v, want a, b, then c with the connection error", got)
	}
	if f.closed == 0 {
		t.Error("the connection must be closed after a connection error")
	}
}

// processDiffItem 在批量模式下只排队文件；类型互换先移除旧类型，再把新文件排队
func TestProcessDiffItemQueuesFiles(t *testing.T) {
	setupFetchQueueRoot(t)
	allow := true
	saved := config.AllowDelete
	config.AllowDelete = &allow
	defer func() { config.AllowDelete = saved }()

	var q fileQueue
	var b tree.Batch
	file := DiffResult{Path: "new.txt", Name: "new.txt", Action: "create", Hash: "h", Size: 3}
	if err := processDiffItem(file, nil, &b, &q); !errors.Is(err, errQueued) {
		t.Fatalf("create: want errQueued, got %v", err)
	}
	old := filepath.Join(config.StartPath, "x")
	if err := os.MkdirAll(filepath.Join(old, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	retype := DiffResult{Path: "x", Name: "x", Action: "retype", Hash: "h", Size: 3}
	if err := processDiffItem(retype, nil, &b, &q); !errors.Is(err, errQueued) {
		t.Fatalf("retype: want errQueued, got %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("retype must remove the old directory before the download is queued: %v", err)
	}
	if len(q) != 2 || q[0].Path != "new.txt" || q[1].Path != "x" {
		t.Errorf("queue %+v", q)
	}
	// 上游读不了的文件不排队
	if err := processDiffItem(DiffResult{Path: "u", Action: "modify"}, nil, &b, &q); err != nil || len(q) != 2 {
		t.Errorf("unreadable upstream file: err %v, queue %d", err, len(q))
	}
}
