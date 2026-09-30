package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

func cmdChart(args []string) error {
	fl := flag.NewFlagSet("chart", flag.ExitOnError)
	outPath := fl.String("out", "", "markdown output file")
	fl.Parse(args)
	if *outPath == "" || fl.NArg() == 0 {
		return errors.New("usage: bench chart -out <file.md> <file.jsonl>...")
	}
	var recs []record
	for _, p := range fl.Args() {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var r record
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				f.Close()
				return fmt.Errorf("%s: %w", p, err)
			}
			recs = append(recs, r)
		}
		f.Close()
	}
	var b strings.Builder
	for _, env := range uniq(recs, func(r record) string { return r.Env }) {
		writeEnv(&b, filter(recs, func(r record) bool { return r.Env == env }))
	}
	return os.WriteFile(*outPath, []byte(b.String()), 0o644)
}

var datasetLabel = map[string]string{
	"small": "small (100,000 × 1–16 KiB)",
	"mixed": "mixed (20,000 files, 1.6 GiB)",
	"large": "large (4 × 1 GiB)",
}

func writeEnv(b *strings.Builder, recs []record) {
	r0 := recs[0]
	fmt.Fprintf(b, "## %s\n\n%s · %s", r0.Env, r0.OS, r0.CPU)
	for _, t := range uniq(recs, func(r record) string { return r.Tool }) {
		v := filter(recs, func(r record) bool { return r.Tool == t })[0].ToolVersion
		fmt.Fprintf(b, " · %s %s", t, v)
	}
	b.WriteString("\n\n")
	tools := uniq(recs, func(r record) string { return r.Tool })
	datasets := uniq(recs, func(r record) string { return r.Dataset })

	b.WriteString("### Initial sync (empty replica)\n\n")
	for _, ds := range datasets {
		vals := make([]float64, len(tools))
		for i, t := range tools {
			vals[i] = median(pluck(recs, t, ds, "initial", "", func(r record) float64 { return r.WallS }))
		}
		writeBar(b, "Initial sync, "+datasetLabel[ds], "seconds", tools, vals)
	}
	writeTable(b, []string{"dataset", "tool", "wall (s)", "CPU (s)", "peak RSS (MiB)"}, func(add func(...string)) {
		for _, ds := range datasets {
			for _, t := range tools {
				rs := pluck(recs, t, ds, "initial", "", nil)
				if len(rs) == 0 {
					continue
				}
				add(ds, t,
					f2(median(pluck(recs, t, ds, "initial", "", func(r record) float64 { return r.WallS }))),
					f2(median(pluck(recs, t, ds, "initial", "", func(r record) float64 { return r.CPUS }))),
					f1(median(pluck(recs, t, ds, "initial", "", func(r record) float64 { return float64(r.PeakRSS) / (1 << 20) }))))
			}
		}
	})

	if hasScenario(recs, "change") {
		b.WriteString("### Propagating one change\n\n")
		b.WriteString("Time from a file being written, modified or deleted in the source until the replica matches. " +
			"For rsync: one run started immediately after the change, which is the lower bound for any rsync schedule.\n\n")
		for _, ds := range datasets {
			if len(filter(recs, func(r record) bool { return r.Dataset == ds && r.Scenario == "change" })) == 0 {
				continue
			}
			vals := make([]float64, len(tools))
			for i, t := range tools {
				vals[i] = median(pluck(recs, t, ds, "change", "", func(r record) float64 { return r.WallS }))
			}
			writeBar(b, "Median latency per change, "+datasetLabel[ds], "seconds", tools, vals)
		}
		writeTable(b, []string{"dataset", "tool", "op", "median (s)", "p95 (s)", "max (s)", "CPU per change (s)", "timeouts"}, func(add func(...string)) {
			for _, ds := range datasets {
				for _, t := range tools {
					for _, op := range []string{"create", "modify", "delete"} {
						rs := sel(recs, t, ds, "change", op)
						if len(rs) == 0 {
							continue
						}
						lat := pluck(recs, t, ds, "change", op, func(r record) float64 { return r.WallS })
						timeouts := 0
						for _, r := range rs {
							if r.TimedOut {
								timeouts++
							}
						}
						add(ds, t, op, f2(median(lat)), f2(percentile(lat, 95)), f2(maxOf(lat)),
							f3(median(pluck(recs, t, ds, "change", op, func(r record) float64 { return r.CPUS }))),
							fmt.Sprintf("%d / %d", timeouts, len(rs)))
					}
				}
			}
		})
	}

	if hasScenario(recs, "idle") || hasScenario(recs, "noop") {
		b.WriteString("### Keeping the replica current\n\n")
		b.WriteString("CPU time per hour with no changes. A continuous tool is measured while idle; " +
			"rsync is one no-op run (replica already identical) multiplied by the runs per hour of a 1-minute schedule.\n\n")
		for _, ds := range datasets {
			var labels []string
			var vals []float64
			for _, t := range tools {
				if idle := pluck(recs, t, ds, "idle", "", nil); len(idle) > 0 {
					per := median(pluck(recs, t, ds, "idle", "", func(r record) float64 { return r.CPUS / r.WallS * 3600 }))
					labels, vals = append(labels, t), append(vals, per)
				}
				if noop := pluck(recs, t, ds, "noop", "", nil); len(noop) > 0 {
					per := median(pluck(recs, t, ds, "noop", "", func(r record) float64 { return r.CPUS })) * 60
					labels, vals = append(labels, t+" every 1 min"), append(vals, per)
				}
			}
			if len(labels) > 0 {
				writeBar(b, "CPU seconds per hour, "+datasetLabel[ds], "CPU seconds", labels, vals)
			}
		}
		writeTable(b, []string{"dataset", "tool", "measured", "wall (s)", "CPU (s)", "CPU per hour (s)", "RSS (MiB)"}, func(add func(...string)) {
			for _, ds := range datasets {
				for _, t := range tools {
					if rs := pluck(recs, t, ds, "idle", "", nil); len(rs) > 0 {
						add(ds, t, "idle",
							f1(median(pluck(recs, t, ds, "idle", "", func(r record) float64 { return r.WallS }))),
							f2(median(pluck(recs, t, ds, "idle", "", func(r record) float64 { return r.CPUS }))),
							f1(median(pluck(recs, t, ds, "idle", "", func(r record) float64 { return r.CPUS / r.WallS * 3600 }))),
							f1(median(pluck(recs, t, ds, "idle", "", func(r record) float64 { return float64(r.AvgRSS) / (1 << 20) }))))
					}
					if rs := pluck(recs, t, ds, "noop", "", nil); len(rs) > 0 {
						add(ds, t, "one no-op run",
							f2(median(pluck(recs, t, ds, "noop", "", func(r record) float64 { return r.WallS }))),
							f2(median(pluck(recs, t, ds, "noop", "", func(r record) float64 { return r.CPUS }))),
							f1(median(pluck(recs, t, ds, "noop", "", func(r record) float64 { return r.CPUS }))*60),
							f1(median(pluck(recs, t, ds, "noop", "", func(r record) float64 { return float64(r.PeakRSS) / (1 << 20) }))))
					}
				}
			}
		})
	}
}

func writeBar(b *strings.Builder, title, unit string, labels []string, vals []float64) {
	quoted := make([]string, len(labels))
	nums := make([]string, len(vals))
	top := 0.0
	for i := range labels {
		quoted[i] = fmt.Sprintf("%q", labels[i])
		nums[i] = strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", vals[i]), "0"), ".")
		top = max(top, vals[i])
	}
	fmt.Fprintf(b, "```mermaid\nxychart-beta\n    title %q\n    x-axis [%s]\n    y-axis %q 0 --> %s\n    bar [%s]\n```\n\n",
		title, strings.Join(quoted, ", "), unit, niceCeil(top*1.1), strings.Join(nums, ", "))
}

func writeTable(b *strings.Builder, header []string, rows func(add func(...string))) {
	fmt.Fprintf(b, "| %s |\n|%s\n", strings.Join(header, " | "), strings.Repeat("---|", len(header)))
	rows(func(cells ...string) { fmt.Fprintf(b, "| %s |\n", strings.Join(cells, " | ")) })
	b.WriteString("\n")
}

func niceCeil(v float64) string {
	if v <= 0 {
		return "1"
	}
	mag := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if m*mag >= v {
			return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", m*mag), "0"), ".")
		}
	}
	return fmt.Sprintf("%g", 10*mag)
}

func pluck(recs []record, tool, ds, scenario, op string, f func(record) float64) []float64 {
	var out []float64
	for _, r := range recs {
		if r.Tool == tool && r.Dataset == ds && r.Scenario == scenario && (op == "" || r.Op == op) {
			if f == nil {
				out = append(out, 0)
			} else {
				out = append(out, f(r))
			}
		}
	}
	return out
}

func sel(recs []record, tool, ds, scenario, op string) []record {
	return filter(recs, func(r record) bool {
		return r.Tool == tool && r.Dataset == ds && r.Scenario == scenario && (op == "" || r.Op == op)
	})
}

func hasScenario(recs []record, s string) bool {
	return len(filter(recs, func(r record) bool { return r.Scenario == s })) > 0
}

func filter(recs []record, keep func(record) bool) []record {
	var out []record
	for _, r := range recs {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

func uniq(recs []record, key func(record) string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range recs {
		if k := key(r); !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func median(v []float64) float64 { return percentile(v, 50) }

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	pos := p / 100 * float64(len(s)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return s[lo] + (s[hi]-s[lo])*(pos-float64(lo))
}

func maxOf(v []float64) float64 {
	m := math.Inf(-1)
	for _, x := range v {
		m = max(m, x)
	}
	return m
}

func f1(v float64) string { return fmt.Sprintf("%.1f", v) }
func f2(v float64) string { return fmt.Sprintf("%.2f", v) }
func f3(v float64) string { return fmt.Sprintf("%.3f", v) }
