package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
)

// tool 被测同步程序。持续型（continuous）启动后常驻、自行跟随变更，由
// pair.stopAll 停止；一次型每次调用 Sync 同步一遍后退出
type tool interface {
	Name() string
	Version(p *pair) string
	Continuous() bool
	// Start 在两端启动持续型程序，进程由各端 agent 计量
	Start(p *pair) error
	// Sync 一次型程序同步一遍，返回耗时与 CPU
	Sync(p *pair) (record, error)
}

// sideVersion 在 s 上执行 bin --version，取第 field 个字段
func sideVersion(s *side, bin string, field int) string {
	var out string
	if err := s.call("exec", []string{bin, "--version"}, &out); err != nil {
		return "unknown"
	}
	f := strings.Fields(strings.SplitN(out, "\n", 2)[0])
	if len(f) <= field {
		return "unknown"
	}
	return strings.TrimPrefix(f[field], "v")
}

// pairVersion 两端版本相同时取一个，不同时写成 源/副本
func pairVersion(p *pair, bin func(*side) string, field int) string {
	a := sideVersion(p.src, bin(p.src), field)
	if p.shared() {
		return a
	}
	if b := sideVersion(p.dst, bin(p.dst), field); b != a {
		return a + "/" + b
	}
	return a
}

// rsyncTool 在本机执行：两端都在本机时是本地复制，否则经 ssh 推到远端或从远端拉
type rsyncTool struct{}

func (rsyncTool) Name() string     { return "rsync" }
func (rsyncTool) Continuous() bool { return false }
func (rsyncTool) Version(p *pair) string {
	return pairVersion(p, func(s *side) string {
		if s.remote {
			return "rsync"
		}
		return s.rsync
	}, 2)
}

func (rsyncTool) Sync(p *pair) (record, error) {
	local, remote := p.src, p.dst
	if p.src.remote {
		local, remote = p.dst, p.src
	}
	src, dst := p.srcP.Src+"/", p.dstP.Dst+"/"
	argv := []string{local.rsync, "-a", "--delete"}
	if remote.remote {
		// 远端的 rsync 套一层脚本记下自己的 CPU
		argv = append(argv, "--rsync-path="+remote.info.RsyncWrapper)
		if remote == p.src {
			src = remote.label + ":" + src
		} else {
			dst = remote.label + ":" + dst
		}
	}
	var res runResult
	if err := local.call("run", procArgs{Argv: append(argv, src, dst)}, &res); err != nil {
		return record{}, err
	}
	rec := record{WallS: res.Wall, CPUS: res.CPU, PeakRSS: res.MaxRSS}
	if remote.remote {
		var cpu float64
		if err := remote.call("rsync_times", nil, &cpu); err != nil {
			return record{}, err
		}
		rec.CPUS += cpu
		if remote == p.src {
			rec.CPUSrc, rec.CPUDst = &cpu, &res.CPU
		} else {
			rec.CPUSrc, rec.CPUDst = &res.CPU, &cpu
		}
	}
	return rec, nil
}

// lmPort local-mirror 监听的第一个端口（每次测试前副本端已无其他实例，必然绑到它）
const lmPort = 52345

// benchKey 两端共用的固定密钥（-k 接受任意字符串，内部派生 PSK）
const benchKey = "local-mirror-bench"

type localMirrorTool struct{}

func (localMirrorTool) Name() string     { return "local-mirror" }
func (localMirrorTool) Continuous() bool { return true }
func (localMirrorTool) Version(p *pair) string {
	return pairVersion(p, func(s *side) string { return s.lm }, 1)
}

func (localMirrorTool) Start(p *pair) error {
	sender := []string{"--send", "-p", p.srcP.Src, "-k", benchKey}
	receiver := []string{"--receive", "-p", p.dstP.Dst, "--allow-delete", "-k", benchKey}
	if p.dstListens() {
		sender = append(sender, "--connect", p.dst.addr)
		receiver = append(receiver, "--listen")
	} else {
		sender = append(sender, "--listen")
		receiver = append(receiver, "--connect", p.src.addr)
	}
	// 监听端先起并等它绑好端口，连接端第一次拨号就能连上
	ends := []struct {
		s    *side
		args []string
		log  string
	}{
		{p.src, sender, filepath.Join(p.srcP.Log, "local-mirror-0.log")},
		{p.dst, receiver, filepath.Join(p.dstP.Log, "local-mirror-1.log")},
	}
	if p.dstListens() {
		ends[0], ends[1] = ends[1], ends[0]
	}
	for i, e := range ends {
		var pid int
		if err := e.s.call("start", procArgs{Argv: append([]string{e.s.lm}, e.args...), Log: e.log, Sig: int(syscall.SIGINT)}, &pid); err != nil {
			return fmt.Errorf("start local-mirror on %s: %w", e.s.label, err)
		}
		if i == 0 {
			if err := waitListen(e.s, lmPort); err != nil {
				return err
			}
		}
	}
	return nil
}

func (localMirrorTool) Sync(*pair) (record, error) { return record{}, fmt.Errorf("continuous") }

func (rsyncTool) Start(*pair) error { return fmt.Errorf("one-shot") }
