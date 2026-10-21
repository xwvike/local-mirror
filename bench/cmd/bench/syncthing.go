package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// syncthingTool 两个 syncthing 实例经回环互连：源端文件夹 sendonly，副本 receiveonly。
// 全局发现、本地广播、中继、NAT、使用统计、崩溃上报与自动升级一律关掉——它们只产生
// 与测试无关的后台流量与 CPU。watchDelay 是文件监控的聚合延迟：默认 10 秒，1 秒为
// 允许的最小值（tuned）
type syncthingTool struct {
	bin        string
	name       string
	watchDelay int
	procs      []*exec.Cmd
	logs       []*os.File
}

// 两个实例的固定端口：同步协议与 GUI（REST）各一个
const (
	stSrcPort, stDstPort       = 22101, 22102
	stSrcGUIPort, stDstGUIPort = 22111, 22112
)

func (t *syncthingTool) Name() string     { return t.name }
func (t *syncthingTool) Continuous() bool { return true }
func (t *syncthingTool) Version() string {
	out, err := exec.Command(t.bin, "--version").Output()
	if err != nil {
		return "unknown"
	}
	f := strings.Fields(string(out))
	if len(f) >= 2 {
		return strings.TrimPrefix(f[1], "v")
	}
	return "unknown"
}

func (t *syncthingTool) Start(src, dst, logDir string) ([]int, error) {
	homes := [2]string{filepath.Join(logDir, "st-src"), filepath.Join(logDir, "st-dst")}
	var ids [2]string
	for i, h := range homes {
		if out, err := exec.Command(t.bin, "generate", "--home="+h, "--no-port-probing").CombinedOutput(); err != nil {
			return nil, fmt.Errorf("syncthing generate: %v: %s", err, out)
		}
		out, err := exec.Command(t.bin, "device-id", "--home="+h).Output()
		if err != nil {
			return nil, fmt.Errorf("syncthing device-id: %w", err)
		}
		ids[i] = strings.TrimSpace(string(out))
	}
	ends := [2]struct {
		path, kind string
		port, gui  int
		self, peer string
		peerPort   int
	}{
		{src, "sendonly", stSrcPort, stSrcGUIPort, ids[0], ids[1], stDstPort},
		{dst, "receiveonly", stDstPort, stDstGUIPort, ids[1], ids[0], stSrcPort},
	}
	var pids []int
	for i, e := range ends {
		// 文件夹标记须事先存在，否则 syncthing 视之为挂载丢失而拒绝同步
		if err := os.MkdirAll(filepath.Join(e.path, ".stfolder"), 0o755); err != nil {
			return nil, err
		}
		cfg := syncthingConfig(e.self, e.peer, e.path, e.kind, e.port, e.peerPort, e.gui, t.watchDelay)
		if err := os.WriteFile(filepath.Join(homes[i], "config.xml"), []byte(cfg), 0o600); err != nil {
			return nil, err
		}
		log, err := os.Create(filepath.Join(logDir, fmt.Sprintf("syncthing-%d.log", i)))
		if err != nil {
			return nil, err
		}
		t.logs = append(t.logs, log)
		cmd := exec.Command(t.bin, "serve", "--home="+homes[i], "--no-browser", "--no-restart")
		cmd.Env = append(os.Environ(), "STNOUPGRADE=1")
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

func syncthingConfig(self, peer, path, kind string, port, peerPort, gui, watchDelay int) string {
	key := make([]byte, 16)
	rand.Read(key)
	device := func(id string, addr string) string {
		return fmt.Sprintf(`    <device id="%s" name="%s" compression="metadata" introducer="false">
        <address>%s</address>
        <autoAcceptFolders>false</autoAcceptFolders>
    </device>
`, id, id[:7], addr)
	}
	return fmt.Sprintf(`<configuration version="52">
    <folder id="bench" label="bench" path="%s" type="%s" rescanIntervalS="3600" fsWatcherEnabled="true" fsWatcherDelayS="%d" ignorePerms="false" autoNormalize="true">
        <filesystemType>basic</filesystemType>
        <device id="%s"></device>
        <device id="%s"></device>
        <minDiskFree unit="%%">1</minDiskFree>
        <markerName>.stfolder</markerName>
    </folder>
%s%s    <gui enabled="true" tls="false">
        <address>127.0.0.1:%d</address>
        <apikey>%s</apikey>
    </gui>
    <options>
        <listenAddress>tcp://127.0.0.1:%d</listenAddress>
        <globalAnnounceEnabled>false</globalAnnounceEnabled>
        <localAnnounceEnabled>false</localAnnounceEnabled>
        <relaysEnabled>false</relaysEnabled>
        <natEnabled>false</natEnabled>
        <startBrowser>false</startBrowser>
        <urAccepted>-1</urAccepted>
        <crashReportingEnabled>false</crashReportingEnabled>
        <autoUpgradeIntervalH>0</autoUpgradeIntervalH>
        <setLowPriority>false</setLowPriority>
    </options>
</configuration>
`, path, kind, watchDelay, self, peer,
		device(self, "dynamic"), device(peer, fmt.Sprintf("tcp://127.0.0.1:%d", peerPort)),
		gui, hex.EncodeToString(key), port)
}

func (t *syncthingTool) Stop() error {
	for _, cmd := range t.procs {
		cmd.Process.Signal(syscall.SIGTERM)
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

func (t *syncthingTool) Sync(string, string, func(int)) (*os.ProcessState, error) {
	return nil, fmt.Errorf("continuous")
}
