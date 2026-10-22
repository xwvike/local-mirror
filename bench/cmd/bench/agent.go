package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"local-mirror/bench/internal/dataset"
)

// agent 一台机器上的测试端：管数据集、起停被测进程、计量、在本机文件系统上
// 施加变更与等待副本。驱动端经 call 调用它的操作：本机直接调用，远端经 ssh
// 以 JSON 逐条收发（bench agent）。两种方式走同一套处理代码
type agent struct {
	ws      string
	mon     *monitor
	procs   []*agentProc
	entries map[string][]dataset.Entry
}

type agentProc struct {
	cmd *exec.Cmd
	log *os.File
	sig syscall.Signal
}

func newAgent(ws string) (*agent, error) {
	abs, err := filepath.Abs(expandHome(ws))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	// rsync 经 ssh 访问本机时用它当 --rsync-path：远端 rsync 的 CPU 不是本进程的
	// 后代，靠 bash 的 times 记到文件里，再由 rsync_times 取走
	script := "#!/bin/bash\nrsync \"$@\"\nrc=$?\ntimes > \"$(dirname \"$0\")/rsync-times\"\nexit $rc\n"
	if err := os.WriteFile(filepath.Join(abs, "rsync-timed.sh"), []byte(script), 0o755); err != nil {
		return nil, err
	}
	return &agent{ws: abs, mon: newMonitor(250 * time.Millisecond), entries: map[string][]dataset.Entry{}}, nil
}

func (a *agent) close() {
	a.stopAll()
	a.mon.Close()
}

type sideInfo struct {
	Host, OS, CPU string
	WS            string
	RsyncWrapper  string
}

type paths struct{ Src, Dst, Log string }

type prepareArgs struct {
	Tool, Dataset string
	Trial         int
	Warm          bool
}

type waitArgs struct {
	Dataset string
	Change  change
	Files   []dataset.Entry
	Tool    string // 副本所在的 run/<tool>
	Sub     string // 数据集内的子路径
	Gone    bool
	Timeout time.Duration
}

type procArgs struct {
	Argv []string
	Env  []string
	Log  string
	Sig  int
}

type runResult struct {
	Wall, CPU float64
	MaxRSS    int64
}

type monResult struct {
	CPU       float64
	Peak, Avg int64
}

// handle 执行一个操作。返回的 error 是操作失败；校验不一致之类的测试结论
// 放在结果里
func (a *agent) handle(op string, raw json.RawMessage) (any, error) {
	var w waitArgs
	switch op {
	case "info":
		host, _ := os.Hostname()
		return sideInfo{Host: host, OS: osDescription(), CPU: cpuDescription(), WS: a.ws, RsyncWrapper: filepath.Join(a.ws, "rsync-timed.sh")}, nil
	case "prepare":
		var p prepareArgs
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		base := filepath.Join(a.ws, "run", p.Tool)
		out := paths{
			Src: a.src(p.Dataset),
			Dst: filepath.Join(base, "dst"),
			Log: filepath.Join(base, "logs", fmt.Sprintf("%s-%d", p.Dataset, p.Trial)),
		}
		if err := os.RemoveAll(base); err != nil {
			return nil, err
		}
		for _, d := range []string{out.Dst, out.Log} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return nil, err
			}
		}
		if p.Warm {
			warmCache(out.Src)
		}
		return out, nil
	case "entries":
		var ds string
		if err := json.Unmarshal(raw, &ds); err != nil {
			return nil, err
		}
		entries, err := dataset.ReadManifest(dataset.ManifestPath(a.src(ds)))
		if err != nil {
			return nil, fmt.Errorf("dataset %s not generated: %w", ds, err)
		}
		a.entries[ds] = entries
		return entries, nil
	case "set_entries":
		var e struct {
			Dataset string
			Entries []dataset.Entry
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, err
		}
		a.entries[e.Dataset] = e.Entries
		return nil, nil
	case "verify_source":
		var ds string
		if err := json.Unmarshal(raw, &ds); err != nil {
			return nil, err
		}
		if err := verifyTree(a.src(ds), a.entries[ds], true); err != nil {
			return nil, fmt.Errorf("source %s is not pristine: %w", a.src(ds), err)
		}
		return nil, nil
	case "apply":
		var c change
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		err := c.apply(a.src(c.Dataset))
		return c, err
	case "restore":
		var r struct {
			Dataset string
			Touched []change
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		spec, err := dataset.Get(r.Dataset)
		if err != nil {
			return nil, err
		}
		return nil, restoreSource(spec, a.src(r.Dataset), r.Touched)
	case "burst_write":
		if err := json.Unmarshal(raw, &w); err != nil {
			return nil, err
		}
		for i, e := range w.Files {
			sum, err := dataset.WriteFile(filepath.Join(a.src(w.Dataset), filepath.FromSlash(e.Path)), "burst/"+e.Path, e.Size)
			if err != nil {
				return nil, err
			}
			w.Files[i].SHA256 = sum
		}
		return w.Files, nil
	case "remove":
		if err := json.Unmarshal(raw, &w); err != nil {
			return nil, err
		}
		return nil, os.RemoveAll(filepath.Join(a.src(w.Dataset), filepath.FromSlash(w.Sub)))
	case "wait_tree", "wait_replica", "wait_burst", "verify_tree", "verify_dir":
		if err := json.Unmarshal(raw, &w); err != nil {
			return nil, err
		}
		dst := filepath.Join(a.ws, "run", w.Tool, "dst")
		switch op {
		case "wait_tree":
			return waitTree(dst, a.entries[w.Dataset], w.Timeout), nil
		case "wait_replica":
			return w.Change.waitReplica(dst, w.Timeout), nil
		case "wait_burst":
			return waitBurst(dst, w.Files, w.Gone, w.Timeout), nil
		case "verify_tree":
			return errString(verifyTree(dst, a.entries[w.Dataset], false)), nil
		default:
			return errString(verifyTree(filepath.Join(dst, filepath.FromSlash(w.Sub)), w.Files, false)), nil
		}
	case "exec":
		var argv []string
		if err := json.Unmarshal(raw, &argv); err != nil {
			return nil, err
		}
		out, err := exec.Command(expandHome(argv[0]), argv[1:]...).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("%s: %v: %s", argv[0], err, out)
		}
		return string(out), nil
	case "write_file":
		var f struct {
			Path string
			Data []byte
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(f.Path, f.Data, 0o600)
	case "mkdir":
		var p string
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return nil, os.MkdirAll(p, 0o755)
	case "start":
		var p procArgs
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return a.start(p)
	case "stop_all":
		a.stopAll()
		return nil, nil
	case "run":
		var p procArgs
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return a.run(p)
	case "mon_reset":
		a.mon.Reset()
		return nil, nil
	case "mon_window":
		cpu, peak, avg := a.mon.Window()
		return monResult{CPU: cpu.Seconds(), Peak: peak, Avg: avg}, nil
	case "wait_listen":
		var l struct {
			Port    int
			Timeout time.Duration
		}
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(l.Timeout)
		for !listening(l.Port) {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("nothing listening on port %d after %v", l.Port, l.Timeout)
			}
			time.Sleep(50 * time.Millisecond)
		}
		return nil, nil
	case "rsync_times":
		return a.rsyncTimes()
	}
	return nil, fmt.Errorf("unknown op %q", op)
}

func (a *agent) src(ds string) string { return filepath.Join(a.ws, "data", ds) }

func errString(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

func (a *agent) start(p procArgs) (int, error) {
	log, err := os.Create(p.Log)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(expandHome(p.Argv[0]), p.Argv[1:]...)
	cmd.Env = append(os.Environ(), p.Env...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		return 0, err
	}
	a.procs = append(a.procs, &agentProc{cmd: cmd, log: log, sig: syscall.Signal(p.Sig)})
	pids := make([]int, len(a.procs))
	for i, pr := range a.procs {
		pids[i] = pr.cmd.Process.Pid
	}
	a.mon.SetRoots(pids...)
	return cmd.Process.Pid, nil
}

func (a *agent) stopAll() {
	for _, p := range a.procs {
		p.cmd.Process.Signal(p.sig)
	}
	for _, p := range a.procs {
		done := make(chan struct{})
		go func() { p.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			p.cmd.Process.Kill()
			<-done
		}
		p.log.Close()
	}
	a.procs = nil
	a.mon.SetRoots()
}

func (a *agent) run(p procArgs) (runResult, error) {
	cmd := exec.Command(expandHome(p.Argv[0]), p.Argv[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	wall := time.Since(start)
	if err != nil {
		return runResult{}, fmt.Errorf("%s: %v: %s", p.Argv[0], err, stderr.String())
	}
	ru := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	cpu := time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	return runResult{Wall: wall.Seconds(), CPU: cpu.Seconds(), MaxRSS: maxRSS(ru)}, nil
}

// rsyncTimes 取走 rsync-timed.sh 记下的远端 rsync CPU：times 第二行是子进程的
// 用户态与内核态时间，形如 0m1.234s 0m0.567s
func (a *agent) rsyncTimes() (float64, error) {
	p := filepath.Join(a.ws, "rsync-times")
	data, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	os.Remove(p)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected times output %q", data)
	}
	var total float64
	for _, f := range strings.Fields(lines[1]) {
		m, s, ok := strings.Cut(strings.TrimSuffix(f, "s"), "m")
		if !ok {
			return 0, fmt.Errorf("unexpected times output %q", data)
		}
		mv, e1 := strconv.ParseFloat(m, 64)
		sv, e2 := strconv.ParseFloat(s, 64)
		if e1 != nil || e2 != nil {
			return 0, fmt.Errorf("unexpected times output %q", data)
		}
		total += mv*60 + sv
	}
	return total, nil
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

type request struct {
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

type response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// cmdAgent 在 stdin/stdout 上逐条处理驱动端的请求。stdin 关闭（驱动端退出、
// ssh 断开）时停掉自己启动的全部进程再退出
func cmdAgent(args []string) error {
	fl := flag.NewFlagSet("agent", flag.ExitOnError)
	ws := fl.String("ws", "", "workspace directory")
	fl.Parse(args)
	if *ws == "" {
		return errors.New("-ws is required")
	}
	a, err := newAgent(*ws)
	if err != nil {
		return err
	}
	defer a.close()
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var resp response
		out, err := a.handle(req.Op, req.Args)
		if err != nil {
			resp.Error = err.Error()
		} else if resp.Result, err = json.Marshal(out); err != nil {
			resp.Error = err.Error()
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
}

// side 驱动端眼里的一端：本机的 agent，或 ssh 那头的 agent
type side struct {
	label  string // 本机为 "local"，远端为 ssh 主机名
	remote bool
	addr   string // 另一端连过来用的地址
	lm, st string // 这台机器上的 local-mirror 与 syncthing
	rsync  string
	info   sideInfo
	call   func(op string, in, out any) error
	close  func()
}

func newLocalSide(ws, lm, st, rsync string) (*side, error) {
	a, err := newAgent(ws)
	if err != nil {
		return nil, err
	}
	s := &side{label: "local", addr: "127.0.0.1", lm: lm, st: st, rsync: rsync, close: a.close}
	// 本机也过一遍 JSON：与远端行为一致，参数没写对立刻暴露
	s.call = func(op string, in, out any) error {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		res, err := a.handle(op, raw)
		if err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		if out == nil {
			return nil
		}
		b, err := json.Marshal(res)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, out)
	}
	return s, s.call("info", nil, &s.info)
}

func newRemoteSide(host, addr, bench, ws, lm, st string) (*side, error) {
	cmd := exec.Command("ssh", "-T", host, fmt.Sprintf("%s agent -ws %s", bench, ws))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = &prefixWriter{prefix: "[" + host + "] ", w: os.Stderr}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var mu sync.Mutex
	enc, dec := json.NewEncoder(stdin), json.NewDecoder(stdout)
	s := &side{label: host, remote: true, addr: addr, lm: lm, st: st, rsync: "rsync"}
	s.call = func(op string, in, out any) error {
		mu.Lock()
		defer mu.Unlock()
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		if err := enc.Encode(request{Op: op, Args: raw}); err != nil {
			return fmt.Errorf("%s on %s: %w", op, host, err)
		}
		var resp response
		if err := dec.Decode(&resp); err != nil {
			return fmt.Errorf("%s on %s: %w", op, host, err)
		}
		if resp.Error != "" {
			return fmt.Errorf("%s on %s: %s", op, host, resp.Error)
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(resp.Result, out)
	}
	s.close = func() {
		stdin.Close()
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	}
	if err := s.call("info", nil, &s.info); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// prefixWriter 给远端 agent 的日志逐行加上主机名
type prefixWriter struct {
	mu     sync.Mutex
	prefix string
	w      io.Writer
	buf    bytes.Buffer
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf.Write(b)
	for {
		line, err := p.buf.ReadBytes('\n')
		if err != nil {
			p.buf.Write(line)
			return len(b), nil
		}
		fmt.Fprint(p.w, p.prefix+string(line))
	}
}

// pair 一次测试的两端。回环时 src 与 dst 是同一个 side
type pair struct {
	src, dst   *side
	srcP, dstP paths
	tool       string
}

func (p *pair) shared() bool { return p.src == p.dst }

func (p *pair) sides() []*side {
	if p.shared() {
		return []*side{p.src}
	}
	return []*side{p.src, p.dst}
}

// dstListens 由哪一端监听：远端那头监听、本机去连（不必为本机开防火墙）；
// 回环时沿用默认，源端监听
func (p *pair) dstListens() bool { return p.dst.remote }

func (p *pair) monReset() error {
	for _, s := range p.sides() {
		if err := s.call("mon_reset", nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// monWindow 两端各自的 CPU 与内存相加；分属两台机器时另记各端 CPU
func (p *pair) monWindow(rec *record) error {
	var per [2]monResult
	for i, s := range p.sides() {
		if err := s.call("mon_window", nil, &per[i]); err != nil {
			return err
		}
	}
	rec.CPUS = per[0].CPU + per[1].CPU
	rec.PeakRSS = per[0].Peak + per[1].Peak
	rec.AvgRSS = per[0].Avg + per[1].Avg
	if !p.shared() {
		rec.CPUSrc, rec.CPUDst = &per[0].CPU, &per[1].CPU
	}
	return nil
}

// waitListen 等 s 上的 port 进入监听，再让对端拨过去：拨号失败会触发
// 被测程序的重连退避，测出来的是退避而不是同步
func waitListen(s *side, port int) error {
	return s.call("wait_listen", map[string]any{"Port": port, "Timeout": 30 * time.Second}, nil)
}

func (p *pair) stopAll() {
	for _, s := range p.sides() {
		if err := s.call("stop_all", nil, nil); err != nil {
			logf("stop on %s: %v", s.label, err)
		}
	}
}

// dstCall 在副本端执行 wait_*/verify_*，定位到该工具的副本目录
func (p *pair) dstCall(op string, w waitArgs, out any) error {
	w.Tool = p.tool
	return p.dst.call(op, w, out)
}
