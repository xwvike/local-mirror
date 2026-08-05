package tree

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"local-mirror/config"
)

// shortTempDir 返回一个路径尽量短的临时目录。unix socket 的 sockaddr_un
// 有长度上限（macOS 约 104 字节），t.TempDir() 给的 /var/folders/... 会超限
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lm")
	if err != nil {
		return t.TempDir()
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestBuildFileTreeSkipsNonRegularFiles socket / FIFO 这类非普通文件没有可复制的
// 字节内容，打开只会报 "operation not supported"。它们必须像符号链接一样被树遍历
// 挡在门外——否则每次重建都记一条 error，还会被登记进不可读列表由恢复循环反复
// 重试，而这类文件永远不会变成可读，条目就此永久滞留。
// 真实触发者：git fsmonitor 守护进程在 .git/fsmonitor--daemon.ipc 留下的 Unix socket
func TestBuildFileTreeSkipsNonRegularFiles(t *testing.T) {
	root := shortTempDir(t)
	config.StartPath = root
	config.IgnoreFileList = nil

	// 普通文件作为对照，必须进树
	if err := os.WriteFile(filepath.Join(root, "regular.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 两类非普通文件各自独立构造，一个不可用不影响另一个
	nonRegular := map[string]string{}

	sockPath := filepath.Join(root, "daemon.ipc")
	if ln, err := net.Listen("unix", sockPath); err == nil {
		defer ln.Close()
		nonRegular["daemon.ipc"] = sockPath
	} else {
		t.Logf("跳过 unix socket 用例: %v", err)
	}

	fifoPath := filepath.Join(root, "pipe.fifo")
	if err := mkfifo(fifoPath); err == nil {
		nonRegular["pipe.fifo"] = fifoPath
	} else {
		t.Logf("跳过 FIFO 用例: %v", err)
	}

	if len(nonRegular) == 0 {
		t.Skip("本平台两类非普通文件都造不出来")
	}

	InitDB()
	defer DB.Close()

	if err := BuildFileTree(root); err != nil {
		t.Fatalf("BuildFileTree 失败: %v", err)
	}

	if ok, err := HasPath("regular.txt"); err != nil || !ok {
		t.Errorf("普通文件必须进树 (ok=%v err=%v)", ok, err)
	}

	registered := make(map[string]bool)
	for _, p := range UnreadableSnapshot() {
		registered[p] = true
	}
	for rel, abs := range nonRegular {
		if ok, _ := HasPath(rel); ok {
			t.Errorf("%s 不该进树", rel)
		}
		// 没进树，自然也不该被登记为"待恢复的不可读文件"
		if registered[abs] {
			t.Errorf("%s 不该登记进不可读列表", rel)
		}
	}
}
