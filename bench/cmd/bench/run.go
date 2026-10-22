package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"local-mirror/bench/internal/dataset"
)

type record struct {
	Env         string   `json:"env"`
	Host        string   `json:"host"`
	OS          string   `json:"os"`
	CPU         string   `json:"cpu"`
	Tool        string   `json:"tool"`
	ToolVersion string   `json:"tool_version"`
	Dataset     string   `json:"dataset"`
	Scenario    string   `json:"scenario"`
	Trial       int      `json:"trial"`
	Op          string   `json:"op,omitempty"`
	WallS       float64  `json:"wall_s"`
	CPUS        float64  `json:"cpu_s"`
	CPUSrc      *float64 `json:"cpu_src_s,omitempty"` // 两端分属两台机器时各自的 CPU
	CPUDst      *float64 `json:"cpu_dst_s,omitempty"`
	PeakRSS     int64    `json:"peak_rss_bytes,omitempty"`
	AvgRSS      int64    `json:"avg_rss_bytes,omitempty"`
	TimedOut    bool     `json:"timed_out,omitempty"`
	Verified    *bool    `json:"verified,omitempty"`
	Time        string   `json:"time"`
}

type runner struct {
	env          string
	trials       int
	changes      int
	idle         time.Duration
	timeout      time.Duration
	opTimeout    time.Duration
	out          *json.Encoder
	pair         *pair
	host, osName string
	cpu          string
	versions     map[string]string
	changeSizes  [2]int64
}

func cmdRun(args []string) error {
	fl := flag.NewFlagSet("run", flag.ExitOnError)
	ws := fl.String("ws", "", "workspace directory (datasets under data/)")
	env := fl.String("env", "loopback", "environment label written into every record")
	toolsFlag := fl.String("tools", "local-mirror,rsync", "tools to run")
	datasets := fl.String("datasets", "small,mixed,large", "datasets to run")
	scenarios := fl.String("scenarios", "initial,steady,change", "initial: empty replica; steady: idle cost (continuous) / no-op run (one-shot); change: per-change latency; burst: 1,000 new files at once, then their removal")
	trials := fl.Int("trials", 3, "trials per tool and dataset")
	changes := fl.Int("changes", 30, "changes per trial in the change scenario")
	idle := fl.Duration("idle", 10*time.Minute, "idle window for continuous tools")
	lmBin := fl.String("local-mirror", "", "local-mirror binary")
	rsyncBin := fl.String("rsync", "rsync", "rsync binary")
	stBin := fl.String("syncthing", "syncthing", "syncthing binary")
	outPath := fl.String("out", "", "JSON Lines output file (appended)")
	remote := fl.String("remote", "", "ssh host running the other end (bench agent); empty: both ends on this machine")
	remoteAddr := fl.String("remote-addr", "", "address of -remote that this machine's tools dial")
	remoteSrc := fl.Bool("remote-src", false, "the remote end is the source (default: the remote end holds the replica)")
	remoteBench := fl.String("remote-bench", "~/lm-bench/bench", "bench binary on -remote")
	remoteWS := fl.String("remote-ws", "~/lm-bench/ws", "workspace on -remote (its datasets when -remote-src)")
	remoteLM := fl.String("remote-local-mirror", "~/lm-bench/local-mirror", "local-mirror binary on -remote")
	remoteST := fl.String("remote-syncthing", "~/lm-bench/syncthing", "syncthing binary on -remote")
	fl.Parse(args)
	if *ws == "" || *outPath == "" {
		return errors.New("-ws and -out are required")
	}
	if *remote != "" && *remoteAddr == "" {
		return errors.New("-remote-addr is required with -remote")
	}

	var tools []tool
	for _, name := range strings.Split(*toolsFlag, ",") {
		switch name {
		case "local-mirror":
			if *lmBin == "" {
				return errors.New("-local-mirror is required")
			}
			tools = append(tools, localMirrorTool{})
		case "rsync":
			tools = append(tools, rsyncTool{})
		case "syncthing":
			tools = append(tools, syncthingTool{name: "syncthing", watchDelay: 10})
		case "syncthing-tuned":
			tools = append(tools, syncthingTool{name: "syncthing-tuned", watchDelay: 1})
		default:
			return fmt.Errorf("unknown tool %q", name)
		}
	}

	f, err := os.OpenFile(*outPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	local, err := newLocalSide(*ws, *lmBin, *stBin, *rsyncBin)
	if err != nil {
		return err
	}
	defer local.close()
	p := &pair{src: local, dst: local}
	if *remote != "" {
		rs, err := newRemoteSide(*remote, *remoteAddr, *remoteBench, *remoteWS, *remoteLM, *remoteST)
		if err != nil {
			return err
		}
		defer rs.close()
		if *remoteSrc {
			p.src = rs
		} else {
			p.dst = rs
		}
	}

	r := &runner{
		env: *env, trials: *trials, changes: *changes, idle: *idle,
		timeout: 2 * time.Hour, opTimeout: 7 * time.Minute,
		out: json.NewEncoder(f), pair: p,
		host:        joinSides(p, func(i sideInfo) string { return i.Host }),
		osName:      joinSides(p, func(i sideInfo) string { return i.OS }),
		cpu:         joinSides(p, func(i sideInfo) string { return i.CPU }),
		versions:    map[string]string{},
		changeSizes: [2]int64{1024, 16384},
	}
	for _, t := range tools {
		r.versions[t.Name()] = t.Version(p)
	}

	sc := map[string]bool{}
	for _, s := range strings.Split(*scenarios, ",") {
		sc[s] = true
	}
	for _, name := range strings.Split(*datasets, ",") {
		spec, err := dataset.Get(name)
		if err != nil {
			return err
		}
		var entries []dataset.Entry
		if err := p.src.call("entries", name, &entries); err != nil {
			return err
		}
		logf("dataset %s: verifying source on %s", name, p.src.label)
		if err := p.src.call("verify_source", name, nil); err != nil {
			return err
		}
		if !p.shared() {
			if err := p.dst.call("set_entries", map[string]any{"Dataset": name, "Entries": entries}, nil); err != nil {
				return err
			}
		}
		for trial := 1; trial <= *trials; trial++ {
			order := tools
			if trial%2 == 0 {
				order = reversed(tools)
			}
			for _, t := range order {
				if err := r.trial(t, spec, entries, trial, sc); err != nil {
					return fmt.Errorf("%s %s trial %d: %w", t.Name(), name, trial, err)
				}
			}
		}
	}
	return nil
}

// joinSides 两端相同时取一个，不同时写成 源 → 副本
func joinSides(p *pair, f func(sideInfo) string) string {
	a, b := f(p.src.info), f(p.dst.info)
	if a == b {
		return a
	}
	return a + " → " + b
}

func (r *runner) emit(t tool, ds string, rec record) {
	rec.Env, rec.Host, rec.OS, rec.CPU = r.env, r.host, r.osName, r.cpu
	rec.Tool, rec.ToolVersion, rec.Dataset = t.Name(), r.versions[t.Name()], ds
	rec.Time = time.Now().UTC().Format(time.RFC3339)
	r.out.Encode(rec)
	extra := ""
	if rec.Op != "" {
		extra = " " + rec.Op
	}
	split := ""
	if rec.CPUSrc != nil {
		split = fmt.Sprintf(" (src %.3f + dst %.3f)", *rec.CPUSrc, *rec.CPUDst)
	}
	logf("  %-12s %-6s %-8s#%d%s wall=%.3fs cpu=%.3fs%s peak=%s avg=%s timeout=%v",
		rec.Tool, ds, rec.Scenario, rec.Trial, extra, rec.WallS, rec.CPUS, split, mib(rec.PeakRSS), mib(rec.AvgRSS), rec.TimedOut)
}

func (r *runner) trial(t tool, spec dataset.Spec, entries []dataset.Entry, trial int, sc map[string]bool) error {
	p := r.pair
	p.tool = t.Name()
	for _, s := range p.sides() {
		var pt paths
		if err := s.call("prepare", prepareArgs{Tool: t.Name(), Dataset: spec.Name, Trial: trial, Warm: s == p.src}, &pt); err != nil {
			return err
		}
		if s == p.src {
			p.srcP = pt
		}
		if s == p.dst {
			p.dstP = pt
		}
	}
	logf("%s %s trial %d", t.Name(), spec.Name, trial)

	var touched []change
	var burstDir string
	defer func() {
		if t.Continuous() {
			p.stopAll()
		}
		if burstDir != "" {
			if err := p.src.call("remove", waitArgs{Dataset: spec.Name, Sub: burstDir}, nil); err != nil {
				logf("remove %s failed: %v", burstDir, err)
			}
		}
		if err := p.src.call("restore", map[string]any{"Dataset": spec.Name, "Touched": touched}, nil); err != nil {
			logf("restore %s failed: %v", spec.Name, err)
		}
	}()
	var ok bool
	waitDst := func(op string, w waitArgs) (bool, error) {
		var ok bool
		err := p.dstCall(op, w, &ok)
		return ok, err
	}
	verifyDst := func(op string, w waitArgs) (bool, error) {
		var msg string
		if err := p.dstCall(op, w, &msg); err != nil {
			return false, err
		}
		if msg != "" {
			logf("  verify: %s", msg)
		}
		return msg == "", nil
	}

	// initial：空副本同步到与源一致
	var rec record
	var err error
	if t.Continuous() {
		if err := p.monReset(); err != nil {
			return err
		}
		start := time.Now()
		if err := t.Start(p); err != nil {
			return err
		}
		if ok, err = waitDst("wait_tree", waitArgs{Dataset: spec.Name, Timeout: r.timeout}); err != nil {
			return err
		}
		rec = record{WallS: time.Since(start).Seconds(), TimedOut: !ok}
		if err := p.monWindow(&rec); err != nil {
			return err
		}
		rec.AvgRSS = 0
	} else if rec, err = t.Sync(p); err != nil {
		return err
	}
	if !rec.TimedOut {
		if ok, err = verifyDst("verify_tree", waitArgs{Dataset: spec.Name}); err != nil {
			return err
		}
	}
	rec.Scenario, rec.Trial, rec.Verified = "initial", trial, &ok
	r.emit(t, spec.Name, rec)
	if !ok {
		return fmt.Errorf("replica does not match the dataset")
	}

	// steady：已一致时的开销
	if sc["steady"] {
		if t.Continuous() {
			time.Sleep(30 * time.Second)
			if err := p.monReset(); err != nil {
				return err
			}
			ws := time.Now()
			time.Sleep(r.idle)
			rec := record{}
			if err := p.monWindow(&rec); err != nil {
				return err
			}
			rec.Scenario, rec.Trial, rec.WallS = "idle", trial, time.Since(ws).Seconds()
			r.emit(t, spec.Name, rec)
		} else {
			rec, err := t.Sync(p)
			if err != nil {
				return err
			}
			rec.Scenario, rec.Trial = "noop", trial
			r.emit(t, spec.Name, rec)
		}
	}

	// change：单个变更从写入源端到副本一致的时间。源端或副本在远端时，
	// 计时含半个往返（局域网几毫秒）
	if sc["change"] && spec.Name != "large" {
		rng := rand.New(rand.NewPCG(uint64(trial), 42))
		used := map[string]bool{}
		jitter := rand.New(rand.NewPCG(uint64(trial), 7)) // 与 rng 分开，不影响各工具拿到的变更序列
		for i := range r.changes {
			c := r.pickChange(spec, entries, rng, used, trial, i)
			if t.Continuous() {
				if err := p.monReset(); err != nil {
					return err
				}
			}
			if err := p.src.call("apply", c, &c); err != nil {
				return err
			}
			touched = append(touched, c)
			t0 := time.Now()
			var rec record
			if t.Continuous() {
				ok, err := waitDst("wait_replica", waitArgs{Change: c, Timeout: r.opTimeout})
				if err != nil {
					return err
				}
				rec = record{WallS: time.Since(t0).Seconds(), TimedOut: !ok}
				if err := p.monWindow(&rec); err != nil {
					return err
				}
				rec.PeakRSS, rec.AvgRSS = 0, 0
			} else {
				var err error
				if rec, err = t.Sync(p); err != nil {
					return err
				}
				if ok, err := waitDst("wait_replica", waitArgs{Change: c}); err != nil || !ok {
					return fmt.Errorf("%s did not propagate %s %s (%v)", t.Name(), c.Op, c.Path, err)
				}
			}
			rec.Scenario, rec.Trial, rec.Op = "change", trial, c.Op
			r.emit(t, spec.Name, rec)
			if t.Continuous() {
				// 间隔带随机零头：固定间隔会让变更总落在秒边界的同一相位，
				// 对按秒处理的程序（local-mirror 的变更日志按秒定型）
				// 测出的永远是同一个偏差，而不是真实分布
				time.Sleep(2*time.Second + time.Duration(jitter.Int64N(int64(time.Second))))
			}
		}
	}

	// burst：一次写入 burstFiles 个新文件（解压、构建产物落盘、git checkout 的形态），
	// 计到副本全部到齐；再整目录删除，计到副本里消失
	if sc["burst"] {
		dir := fmt.Sprintf("burst-%d", trial)
		burstDir = dir
		rng := rand.New(rand.NewPCG(uint64(trial), 99))
		files := make([]dataset.Entry, burstFiles)
		for i := range files {
			files[i].Path = fmt.Sprintf("%s/d%02d/b%04d.bin", dir, i/100, i)
			files[i].Size = r.changeSizes[0] + rng.Int64N(r.changeSizes[1]-r.changeSizes[0]+1)
		}
		for _, op := range []string{"create", "delete"} {
			if t.Continuous() {
				if err := p.monReset(); err != nil {
					return err
				}
			}
			t0 := time.Now()
			if op == "delete" {
				if err := p.src.call("remove", waitArgs{Dataset: spec.Name, Sub: dir}, nil); err != nil {
					return err
				}
			} else if err := p.src.call("burst_write", waitArgs{Dataset: spec.Name, Files: files}, &files); err != nil {
				return err
			}
			w := waitArgs{Files: files, Gone: op == "delete"}
			var rec record
			if t.Continuous() {
				w.Timeout = r.opTimeout
				ok, err := waitDst("wait_burst", w)
				if err != nil {
					return err
				}
				rec = record{WallS: time.Since(t0).Seconds(), TimedOut: !ok}
				if err := p.monWindow(&rec); err != nil {
					return err
				}
				rec.PeakRSS, rec.AvgRSS = 0, 0
			} else {
				var err error
				if rec, err = t.Sync(p); err != nil {
					return err
				}
				rec.WallS = time.Since(t0).Seconds()
				if ok, err := waitDst("wait_burst", w); err != nil || !ok {
					return fmt.Errorf("%s did not propagate burst %s (%v)", t.Name(), op, err)
				}
			}
			if op == "create" && !rec.TimedOut {
				ok, err := verifyDst("verify_dir", waitArgs{Sub: dir, Files: rebase(files, dir)})
				if err != nil {
					return err
				}
				rec.Verified = &ok
			}
			rec.Scenario, rec.Trial, rec.Op = "burst", trial, op
			r.emit(t, spec.Name, rec)
			if t.Continuous() {
				time.Sleep(3 * time.Second)
			}
		}
	}
	return nil
}

// burstFiles burst 场景一次写入的文件数
const burstFiles = 1000

// waitBurst 等 burst 的文件全部出现在副本里（gone 时等它们全部消失）
func waitBurst(dst string, files []dataset.Entry, gone bool, timeout time.Duration) bool {
	if !gone {
		return waitTree(dst, files, timeout)
	}
	deadline := time.Now().Add(timeout)
	for {
		left := false
		for _, e := range files {
			if _, err := os.Lstat(filepath.Join(dst, filepath.FromSlash(e.Path))); err == nil {
				left = true
				break
			}
		}
		if !left {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// rebase 把条目路径改成相对 dir 的形式，供 verifyTree 校验子树
func rebase(files []dataset.Entry, dir string) []dataset.Entry {
	out := make([]dataset.Entry, len(files))
	for i, e := range files {
		out[i] = e
		out[i].Path = strings.TrimPrefix(e.Path, dir+"/")
	}
	return out
}

type change struct {
	Dataset  string
	Op, Path string
	Size     int64
	Key, Sum string
}

func (r *runner) pickChange(spec dataset.Spec, entries []dataset.Entry, rng *rand.Rand, used map[string]bool, trial, i int) change {
	pick := func() dataset.Entry {
		for {
			e := entries[rng.IntN(len(entries))]
			if !used[e.Path] {
				used[e.Path] = true
				return e
			}
		}
	}
	size := r.changeSizes[0] + rng.Int64N(r.changeSizes[1]-r.changeSizes[0]+1)
	key := fmt.Sprintf("change/%s/%d/%d", spec.Name, trial, i)
	switch i % 3 {
	case 0:
		e := pick()
		return change{Dataset: spec.Name, Op: "create", Path: filepath.ToSlash(filepath.Join(filepath.Dir(e.Path), fmt.Sprintf("new-%d-%d.bin", trial, i))), Size: size, Key: key}
	case 1:
		return change{Dataset: spec.Name, Op: "modify", Path: pick().Path, Size: size, Key: key}
	default:
		return change{Dataset: spec.Name, Op: "delete", Path: pick().Path}
	}
}

func (c *change) apply(src string) error {
	p := filepath.Join(src, filepath.FromSlash(c.Path))
	if c.Op == "delete" {
		return os.Remove(p)
	}
	sum, err := dataset.WriteFile(p, c.Key, c.Size)
	c.Sum = sum
	return err
}

func (c change) waitReplica(dst string, timeout time.Duration) bool {
	p := filepath.Join(dst, filepath.FromSlash(c.Path))
	deadline := time.Now().Add(timeout)
	var lastSize int64 = -1
	var lastMod time.Time
	for {
		fi, err := os.Lstat(p)
		switch {
		case c.Op == "delete":
			if errors.Is(err, fs.ErrNotExist) {
				return true
			}
		case err == nil && fi.Mode().IsRegular() && fi.Size() == c.Size:
			if fi.Size() != lastSize || !fi.ModTime().Equal(lastMod) {
				lastSize, lastMod = fi.Size(), fi.ModTime()
				if s, err := fileSHA256(p); err == nil && s == c.Sum {
					return true
				}
			}
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// restoreSource 撤销变更场景对源数据集的改动，并清掉被测程序写入的状态目录
func restoreSource(spec dataset.Spec, src string, touched []change) error {
	for _, d := range toolStateDirs {
		if err := os.RemoveAll(filepath.Join(src, d)); err != nil {
			return err
		}
	}
	sizes := map[string]int64{}
	for _, f := range spec.Files() {
		sizes[f.Path] = f.Size
	}
	for _, c := range touched {
		p := filepath.Join(src, filepath.FromSlash(c.Path))
		orig, existed := sizes[c.Path]
		if !existed {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		if _, err := dataset.WriteFile(p, spec.Name+"/"+c.Path, orig); err != nil {
			return err
		}
		if err := os.Chtimes(p, dataset.MTime, dataset.MTime); err != nil {
			return err
		}
	}
	return nil
}

// waitTree 等副本中出现数据集的全部文件且大小一致。每轮只检查有限个待定文件，
// 控制轮询本身的开销
func waitTree(dst string, entries []dataset.Entry, timeout time.Duration) bool {
	pending := make([]dataset.Entry, len(entries))
	copy(pending, entries)
	deadline := time.Now().Add(timeout)
	next, swept := 0, 0
	for {
		checked := 0
		for checked < 500 && len(pending) > 0 {
			if next >= len(pending) {
				next = 0
			}
			e := pending[next]
			fi, err := os.Lstat(filepath.Join(dst, filepath.FromSlash(e.Path)))
			if err == nil && fi.Mode().IsRegular() && fi.Size() == e.Size {
				pending[next] = pending[len(pending)-1]
				pending = pending[:len(pending)-1]
			} else {
				next++
			}
			checked++
		}
		if len(pending) == 0 {
			return true
		}
		// 先查后判超时：timeout 为 0 时也至少查满一轮（一次性工具跑完后的核对）
		if swept += checked; swept >= len(entries) && time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// toolStateDirs 被测程序在同步根里放的状态目录，不属于数据集
var toolStateDirs = []string{".local-mirror", ".stfolder", ".stversions"}

func isToolStateDir(rel string) bool {
	for _, d := range toolStateDirs {
		if rel == d {
			return true
		}
	}
	return false
}

// verifyTree 逐文件比对内容，并确认没有多余文件（被测程序的状态目录除外）
func verifyTree(root string, entries []dataset.Entry, strictExtras bool) error {
	want := make(map[string]dataset.Entry, len(entries))
	for _, e := range entries {
		want[e.Path] = e
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if isToolStateDir(rel) && !strictExtras {
				return filepath.SkipDir
			}
			return nil
		}
		e, ok := want[rel]
		if !ok {
			return fmt.Errorf("unexpected file %s", rel)
		}
		delete(want, rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Size() != e.Size {
			return fmt.Errorf("%s: size %d, want %d", rel, fi.Size(), e.Size)
		}
		sum, err := fileSHA256(p)
		if err != nil {
			return err
		}
		if sum != e.SHA256 {
			return fmt.Errorf("%s: content mismatch", rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for p := range want {
		return fmt.Errorf("missing file %s (and %d more)", p, len(want)-1)
	}
	return nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// warmCache 读一遍源数据，让每次测试都从热页缓存开始
func warmCache(root string) {
	buf := make([]byte, 1<<20)
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if f, err := os.Open(p); err == nil {
			for {
				if _, err := f.Read(buf); err != nil {
					break
				}
			}
			f.Close()
		}
		return nil
	})
}

func reversed(ts []tool) []tool {
	out := make([]tool, len(ts))
	for i, t := range ts {
		out[len(ts)-1-i] = t
	}
	return out
}

func osDescription() string {
	switch runtime.GOOS {
	case "darwin":
		v, _ := exec.Command("sw_vers", "-productVersion").Output()
		return "macOS " + strings.TrimSpace(string(v))
	case "linux":
		data, _ := os.ReadFile("/etc/os-release")
		for _, l := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
				return strings.Trim(v, `"`)
			}
		}
	}
	return runtime.GOOS
}

func cpuDescription() string {
	switch runtime.GOOS {
	case "darwin":
		v, _ := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
		return fmt.Sprintf("%s, %d cores", strings.TrimSpace(string(v)), runtime.NumCPU())
	case "linux":
		data, _ := os.ReadFile("/proc/cpuinfo")
		for _, l := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(l, "model name") {
				return fmt.Sprintf("%s, %d cores", strings.TrimSpace(strings.SplitN(l, ":", 2)[1]), runtime.NumCPU())
			}
		}
	}
	return runtime.GOARCH
}

func mib(b int64) string {
	if b == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1fMiB", float64(b)/(1<<20))
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05 ")+format+"\n", a...)
}
