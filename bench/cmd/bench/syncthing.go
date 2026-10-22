package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
)

// syncthingTool 两个 syncthing 实例互连（回环或局域网）：源端文件夹 sendonly，副本 receiveonly。
// 全局发现、本地广播、中继、NAT、使用统计、崩溃上报与自动升级一律关掉——它们只产生
// 与测试无关的后台流量与 CPU。watchDelay 是文件监控的聚合延迟：默认 10 秒，1 秒为
// 允许的最小值（tuned）
type syncthingTool struct {
	name       string
	watchDelay int
}

// 两个实例的固定端口：同步协议与 GUI（REST）各一个
const (
	stSrcPort, stDstPort       = 22101, 22102
	stSrcGUIPort, stDstGUIPort = 22111, 22112
)

func (t syncthingTool) Name() string     { return t.name }
func (t syncthingTool) Continuous() bool { return true }
func (t syncthingTool) Version(p *pair) string {
	return pairVersion(p, func(s *side) string { return s.st }, 1)
}

func (t syncthingTool) Start(p *pair) error {
	type end struct {
		s          *side
		path, kind string
		home, log  string
		port, gui  int
	}
	ends := [2]end{
		{p.src, p.srcP.Src, "sendonly", filepath.Join(p.srcP.Log, "st-src"), filepath.Join(p.srcP.Log, "syncthing-0.log"), stSrcPort, stSrcGUIPort},
		{p.dst, p.dstP.Dst, "receiveonly", filepath.Join(p.dstP.Log, "st-dst"), filepath.Join(p.dstP.Log, "syncthing-1.log"), stDstPort, stDstGUIPort},
	}
	var ids [2]string
	for i, e := range ends {
		if err := e.s.call("exec", []string{e.s.st, "generate", "--home=" + e.home, "--no-port-probing"}, nil); err != nil {
			return err
		}
		var out string
		if err := e.s.call("exec", []string{e.s.st, "device-id", "--home=" + e.home}, &out); err != nil {
			return err
		}
		ids[i] = strings.TrimSpace(out)
	}
	// 远端实例只监听，先起并等它绑好端口
	order := []int{0, 1}
	if p.dst.remote {
		order = []int{1, 0}
	}
	for _, i := range order {
		e := ends[i]
		peer := ends[1-i]
		// 本机的实例去连对端；远端的实例只监听，对端地址留 dynamic（发现已关，
		// 等于不主动连），本机不必为它开防火墙
		listen, peerAddr := "127.0.0.1", fmt.Sprintf("tcp://%s:%d", peer.s.addr, peer.port)
		if e.s.remote {
			listen, peerAddr = "0.0.0.0", "dynamic"
		}
		// 文件夹标记须事先存在，否则 syncthing 视之为挂载丢失而拒绝同步
		if err := e.s.call("mkdir", filepath.Join(e.path, ".stfolder"), nil); err != nil {
			return err
		}
		cfg := syncthingConfig(ids[i], ids[1-i], e.path, e.kind, listen, e.port, peerAddr, e.gui, t.watchDelay)
		if err := e.s.call("write_file", map[string]any{"Path": filepath.Join(e.home, "config.xml"), "Data": []byte(cfg)}, nil); err != nil {
			return err
		}
		var pid int
		argv := []string{e.s.st, "serve", "--home=" + e.home, "--no-browser", "--no-restart"}
		if err := e.s.call("start", procArgs{Argv: argv, Env: []string{"STNOUPGRADE=1"}, Log: e.log, Sig: int(syscall.SIGTERM)}, &pid); err != nil {
			return fmt.Errorf("start syncthing on %s: %w", e.s.label, err)
		}
		if e.s.remote {
			if err := waitListen(e.s, e.port); err != nil {
				return err
			}
		}
	}
	return nil
}

func syncthingConfig(self, peer, path, kind, listen string, port int, peerAddr string, gui, watchDelay int) string {
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
        <listenAddress>tcp://%s:%d</listenAddress>
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
		device(self, "dynamic"), device(peer, peerAddr),
		gui, hex.EncodeToString(key), listen, port)
}

func (t syncthingTool) Sync(*pair) (record, error) { return record{}, fmt.Errorf("continuous") }
