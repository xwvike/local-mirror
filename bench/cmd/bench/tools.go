package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// tool 被测同步程序。持续型（continuous）启动后常驻、自行跟随变更；
// 一次型每次调用 Sync 同步一遍后退出
type tool interface {
	Name() string
	Version() string
	Continuous() bool
	// Start 启动持续型程序，返回需要计量的进程
	Start(src, dst, logDir string) ([]int, error)
	Stop() error
	// Sync 一次型程序同步一遍；started 在进程启动后回调，供计量挂上进程
	Sync(src, dst string, started func(pid int)) (*os.ProcessState, error)
}

type rsyncTool struct{ bin string }

func (t rsyncTool) Name() string     { return "rsync" }
func (t rsyncTool) Continuous() bool { return false }
func (t rsyncTool) Version() string {
	out, err := exec.Command(t.bin, "--version").Output()
	if err != nil {
		return "unknown"
	}
	f := strings.Fields(strings.SplitN(string(out), "\n", 2)[0])
	if len(f) >= 3 {
		return f[2]
	}
	return "unknown"
}
func (t rsyncTool) Start(string, string, string) ([]int, error) { return nil, fmt.Errorf("one-shot") }
func (t rsyncTool) Stop() error                                 { return nil }
func (t rsyncTool) Sync(src, dst string, started func(int)) (*os.ProcessState, error) {
	cmd := exec.Command(t.bin, "-a", "--delete", src+"/", dst+"/")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	started(cmd.Process.Pid)
	if err := cmd.Wait(); err != nil {
		return cmd.ProcessState, fmt.Errorf("rsync: %v: %s", err, stderr.String())
	}
	return cmd.ProcessState, nil
}

// benchKey 两端共用的固定密钥（-k 接受任意字符串，内部派生 PSK）
const benchKey = "local-mirror-bench"

type localMirrorTool struct {
	bin   string
	procs []*exec.Cmd
	logs  []*os.File
}

func (t *localMirrorTool) Name() string     { return "local-mirror" }
func (t *localMirrorTool) Continuous() bool { return true }
func (t *localMirrorTool) Version() string {
	out, err := exec.Command(t.bin, "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimPrefix(strings.Fields(string(out) + " unknown")[1], "v")
}

func (t *localMirrorTool) Start(src, dst, logDir string) ([]int, error) {
	sender := []string{"--send", "-p", src, "-k", benchKey}
	receiver := []string{"--receive", "--connect", "127.0.0.1", "-p", dst, "--allow-delete", "-k", benchKey}
	var pids []int
	for i, args := range [][]string{sender, receiver} {
		log, err := os.Create(filepath.Join(logDir, fmt.Sprintf("local-mirror-%d.log", i)))
		if err != nil {
			return nil, err
		}
		t.logs = append(t.logs, log)
		cmd := exec.Command(t.bin, args...)
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			t.Stop()
			return nil, err
		}
		t.procs = append(t.procs, cmd)
		pids = append(pids, cmd.Process.Pid)
	}
	return pids, nil
}

func (t *localMirrorTool) Stop() error {
	for _, cmd := range t.procs {
		cmd.Process.Signal(syscall.SIGINT)
	}
	for _, cmd := range t.procs {
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	}
	for _, f := range t.logs {
		f.Close()
	}
	t.procs, t.logs = nil, nil
	return nil
}

func (t *localMirrorTool) Sync(string, string, func(int)) (*os.ProcessState, error) {
	return nil, fmt.Errorf("continuous")
}
