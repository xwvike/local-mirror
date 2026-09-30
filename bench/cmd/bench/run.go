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
	"syscall"
	"time"

	"local-mirror/bench/internal/dataset"
)

type record struct {
	Env         string  `json:"env"`
	Host        string  `json:"host"`
	OS          string  `json:"os"`
	CPU         string  `json:"cpu"`
	Tool        string  `json:"tool"`
	ToolVersion string  `json:"tool_version"`
	Dataset     string  `json:"dataset"`
	Scenario    string  `json:"scenario"`
	Trial       int     `json:"trial"`
	Op          string  `json:"op,omitempty"`
	WallS       float64 `json:"wall_s"`
	CPUS        float64 `json:"cpu_s"`
	PeakRSS     int64   `json:"peak_rss_bytes,omitempty"`
	AvgRSS      int64   `json:"avg_rss_bytes,omitempty"`
	TimedOut    bool    `json:"timed_out,omitempty"`
	Verified    *bool   `json:"verified,omitempty"`
	Time        string  `json:"time"`
}

type runner struct {
	ws, env      string
	trials       int
	changes      int
	idle         time.Duration
	timeout      time.Duration
	opTimeout    time.Duration
	out          *json.Encoder
	mon          *monitor
	host, osName string
	cpu          string
	changeSizes  [2]int64
}

func cmdRun(args []string) error {
	fl := flag.NewFlagSet("run", flag.ExitOnError)
	ws := fl.String("ws", "", "workspace directory (datasets under data/)")
	env := fl.String("env", "loopback", "environment label written into every record")
	toolsFlag := fl.String("tools", "local-mirror,rsync", "tools to run")
	datasets := fl.String("datasets", "small,mixed,large", "datasets to run")
	scenarios := fl.String("scenarios", "initial,steady,change", "initial: empty replica; steady: idle cost (continuous) / no-op run (one-shot); change: per-change latency")
	trials := fl.Int("trials", 3, "trials per tool and dataset")
	changes := fl.Int("changes", 30, "changes per trial in the change scenario")
	idle := fl.Duration("idle", 10*time.Minute, "idle window for continuous tools")
	lmBin := fl.String("local-mirror", "", "local-mirror binary")
	rsyncBin := fl.String("rsync", "rsync", "rsync binary")
	outPath := fl.String("out", "", "JSON Lines output file (appended)")
	fl.Parse(args)
	if *ws == "" || *outPath == "" {
		return errors.New("-ws and -out are required")
	}

	var tools []tool
	for _, name := range strings.Split(*toolsFlag, ",") {
		switch name {
		case "local-mirror":
			if *lmBin == "" {
				return errors.New("-local-mirror is required")
			}
			tools = append(tools, &localMirrorTool{bin: *lmBin})
		case "rsync":
			tools = append(tools, rsyncTool{bin: *rsyncBin})
		default:
			return fmt.Errorf("unknown tool %q", name)
		}
	}

	f, err := os.OpenFile(*outPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	host, _ := os.Hostname()
	r := &runner{
		ws: *ws, env: *env, trials: *trials, changes: *changes, idle: *idle,
		timeout: 2 * time.Hour, opTimeout: 7 * time.Minute,
		out: json.NewEncoder(f), mon: newMonitor(250 * time.Millisecond),
		host: host, osName: osDescription(), cpu: cpuDescription(),
		changeSizes: [2]int64{1024, 16384},
	}
	defer r.mon.Close()

	sc := map[string]bool{}
	for _, s := range strings.Split(*scenarios, ",") {
		sc[s] = true
	}
	for _, name := range strings.Split(*datasets, ",") {
		spec, err := dataset.Get(name)
		if err != nil {
			return err
		}
		src := filepath.Join(*ws, "data", name)
		entries, err := dataset.ReadManifest(dataset.ManifestPath(src))
		if err != nil {
			return fmt.Errorf("dataset %s not generated: %w", name, err)
		}
		logf("dataset %s: verifying source", name)
		if err := verifyTree(src, entries, true); err != nil {
			return fmt.Errorf("source %s is not pristine: %w", src, err)
		}
		for trial := 1; trial <= *trials; trial++ {
			order := tools
			if trial%2 == 0 {
				order = reversed(tools)
			}
			for _, t := range order {
				if err := r.trial(t, spec, src, entries, trial, sc); err != nil {
					return fmt.Errorf("%s %s trial %d: %w", t.Name(), name, trial, err)
				}
			}
		}
	}
	return nil
}

func (r *runner) emit(t tool, ds string, rec record) {
	rec.Env, rec.Host, rec.OS, rec.CPU = r.env, r.host, r.osName, r.cpu
	rec.Tool, rec.ToolVersion, rec.Dataset = t.Name(), t.Version(), ds
	rec.Time = time.Now().UTC().Format(time.RFC3339)
	r.out.Encode(rec)
	extra := ""
	if rec.Op != "" {
		extra = " " + rec.Op
	}
	logf("  %-12s %-6s %-8s#%d%s wall=%.3fs cpu=%.3fs peak=%s avg=%s timeout=%v",
		rec.Tool, ds, rec.Scenario, rec.Trial, extra, rec.WallS, rec.CPUS, mib(rec.PeakRSS), mib(rec.AvgRSS), rec.TimedOut)
}

func (r *runner) trial(t tool, spec dataset.Spec, src string, entries []dataset.Entry, trial int, sc map[string]bool) error {
	base := filepath.Join(r.ws, "run", t.Name())
	dst := filepath.Join(base, "dst")
	logDir := filepath.Join(base, "logs", fmt.Sprintf("%s-%d", spec.Name, trial))
	if err := os.RemoveAll(base); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	warmCache(src)
	logf("%s %s trial %d", t.Name(), spec.Name, trial)

	var touched []change
	defer func() {
		if t.Continuous() {
			t.Stop()
		}
		if err := restoreSource(spec, src, touched); err != nil {
			logf("restore %s failed: %v", src, err)
		}
	}()

	// initial：空副本同步到与源一致
	start := time.Now()
	if t.Continuous() {
		r.mon.SetRoots()
		r.mon.Reset()
		pids, err := t.Start(src, dst, logDir)
		if err != nil {
			return err
		}
		r.mon.SetRoots(pids...)
		timedOut := !waitTree(dst, entries, r.timeout)
		wall := time.Since(start)
		cpu, peak, _ := r.mon.Window()
		ok := !timedOut && verifyTree(dst, entries, false) == nil
		r.emit(t, spec.Name, record{Scenario: "initial", Trial: trial, WallS: wall.Seconds(), CPUS: cpu.Seconds(), PeakRSS: peak, TimedOut: timedOut, Verified: &ok})
		if !ok {
			return fmt.Errorf("replica does not match the dataset")
		}
	} else {
		rec, err := r.oneShot(t, src, dst)
		if err != nil {
			return err
		}
		ok := verifyTree(dst, entries, false) == nil
		rec.Scenario, rec.Trial, rec.Verified = "initial", trial, &ok
		r.emit(t, spec.Name, rec)
		if !ok {
			return fmt.Errorf("replica does not match the dataset")
		}
	}

	// steady：已一致时的开销
	if sc["steady"] {
		if t.Continuous() {
			time.Sleep(30 * time.Second)
			r.mon.Reset()
			ws := time.Now()
			time.Sleep(r.idle)
			cpu, peak, avg := r.mon.Window()
			r.emit(t, spec.Name, record{Scenario: "idle", Trial: trial, WallS: time.Since(ws).Seconds(), CPUS: cpu.Seconds(), PeakRSS: peak, AvgRSS: avg})
		} else {
			rec, err := r.oneShot(t, src, dst)
			if err != nil {
				return err
			}
			rec.Scenario, rec.Trial = "noop", trial
			r.emit(t, spec.Name, rec)
		}
	}

	// change：单个变更从写入源端到副本一致的时间
	if sc["change"] && spec.Name != "large" {
		rng := rand.New(rand.NewPCG(uint64(trial), 42))
		used := map[string]bool{}
		for i := range r.changes {
			c := r.pickChange(spec, entries, rng, used, trial, i)
			if err := c.apply(src); err != nil {
				return err
			}
			touched = append(touched, c)
			t0 := time.Now()
			var rec record
			if t.Continuous() {
				r.mon.Reset()
				ok := c.waitReplica(dst, r.opTimeout)
				cpu, _, _ := r.mon.Window()
				rec = record{WallS: time.Since(t0).Seconds(), CPUS: cpu.Seconds(), TimedOut: !ok}
			} else {
				var err error
				if rec, err = r.oneShot(t, src, dst); err != nil {
					return err
				}
				if !c.waitReplica(dst, 0) {
					return fmt.Errorf("%s did not propagate %s %s", t.Name(), c.op, c.path)
				}
			}
			rec.Scenario, rec.Trial, rec.Op = "change", trial, c.op
			r.emit(t, spec.Name, rec)
			if t.Continuous() {
				time.Sleep(2 * time.Second)
			}
		}
	}
	return nil
}

func (r *runner) oneShot(t tool, src, dst string) (record, error) {
	r.mon.SetRoots()
	r.mon.Reset()
	start := time.Now()
	st, err := t.Sync(src, dst, func(pid int) { r.mon.SetRoots(pid) })
	wall := time.Since(start)
	if err != nil {
		return record{}, err
	}
	_, peak, _ := r.mon.Window()
	ru := st.SysUsage().(*syscall.Rusage)
	cpu := time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	return record{WallS: wall.Seconds(), CPUS: cpu.Seconds(), PeakRSS: max(peak, maxRSS(ru))}, nil
}

type change struct {
	op, path string
	size     int64
	key      string
	sum      string
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
		return change{op: "create", path: filepath.ToSlash(filepath.Join(filepath.Dir(e.Path), fmt.Sprintf("new-%d-%d.bin", trial, i))), size: size, key: key}
	case 1:
		return change{op: "modify", path: pick().Path, size: size, key: key}
	default:
		return change{op: "delete", path: pick().Path}
	}
}

func (c *change) apply(src string) error {
	p := filepath.Join(src, filepath.FromSlash(c.path))
	if c.op == "delete" {
		return os.Remove(p)
	}
	sum, err := dataset.WriteFile(p, c.key, c.size)
	c.sum = sum
	return err
}

func (c change) waitReplica(dst string, timeout time.Duration) bool {
	p := filepath.Join(dst, filepath.FromSlash(c.path))
	deadline := time.Now().Add(timeout)
	var lastSize int64 = -1
	var lastMod time.Time
	for {
		fi, err := os.Lstat(p)
		switch {
		case c.op == "delete":
			if errors.Is(err, fs.ErrNotExist) {
				return true
			}
		case err == nil && fi.Mode().IsRegular() && fi.Size() == c.size:
			if fi.Size() != lastSize || !fi.ModTime().Equal(lastMod) {
				lastSize, lastMod = fi.Size(), fi.ModTime()
				if s, err := fileSHA256(p); err == nil && s == c.sum {
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
	if err := os.RemoveAll(filepath.Join(src, ".local-mirror")); err != nil {
		return err
	}
	sizes := map[string]int64{}
	for _, f := range spec.Files() {
		sizes[f.Path] = f.Size
	}
	for _, c := range touched {
		p := filepath.Join(src, filepath.FromSlash(c.path))
		orig, existed := sizes[c.path]
		if !existed {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			continue
		}
		if _, err := dataset.WriteFile(p, spec.Name+"/"+c.path, orig); err != nil {
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
	next := 0
	for len(pending) > 0 {
		if time.Now().After(deadline) {
			return false
		}
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
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

// verifyTree 逐文件比对内容，并确认没有多余文件（被测程序的 .local-mirror 除外）
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
			if rel == ".local-mirror" && !strictExtras {
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
