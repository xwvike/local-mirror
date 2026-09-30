package main

import (
	"sync"
	"time"
)

type procInfo struct {
	PPID int
	RSS  int64
	CPU  time.Duration
}

// monitor 周期采样指定根进程及其全部后代，累计 CPU 时间与 RSS 之和
type monitor struct {
	mu       sync.Mutex
	roots    map[int]bool
	baseline map[int]time.Duration
	last     map[int]time.Duration
	peakRSS  int64
	rssSum   float64
	rssN     int
	stop     chan struct{}
	done     chan struct{}
}

func newMonitor(interval time.Duration) *monitor {
	m := &monitor{
		roots:    map[int]bool{},
		baseline: map[int]time.Duration{},
		last:     map[int]time.Duration{},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go m.loop(interval)
	return m
}

func (m *monitor) loop(interval time.Duration) {
	defer close(m.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.Sample()
		}
	}
}

func (m *monitor) Close() {
	close(m.stop)
	<-m.done
}

func (m *monitor) SetRoots(pids ...int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roots = map[int]bool{}
	for _, p := range pids {
		m.roots[p] = true
	}
}

// Reset 以当前累计值为基线，开始新的计量窗口
func (m *monitor) Reset() {
	m.Sample()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.baseline = map[int]time.Duration{}
	for pid, cpu := range m.last {
		m.baseline[pid] = cpu
	}
	m.peakRSS, m.rssSum, m.rssN = 0, 0, 0
}

// Window 返回自 Reset 以来的 CPU 时间、峰值 RSS 与平均 RSS
func (m *monitor) Window() (cpu time.Duration, peak, avg int64) {
	m.Sample()
	m.mu.Lock()
	defer m.mu.Unlock()
	for pid, c := range m.last {
		cpu += c - m.baseline[pid]
	}
	if m.rssN > 0 {
		avg = int64(m.rssSum / float64(m.rssN))
	}
	return cpu, m.peakRSS, avg
}

func (m *monitor) Sample() {
	procs, err := snapshotProcs()
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.roots) == 0 {
		return
	}
	children := map[int][]int{}
	for pid, p := range procs {
		children[p.PPID] = append(children[p.PPID], pid)
	}
	var rss int64
	var walk func(int)
	walk = func(pid int) {
		p, ok := procs[pid]
		if !ok {
			return
		}
		rss += p.RSS
		if p.CPU > m.last[pid] {
			m.last[pid] = p.CPU
		}
		for _, c := range children[pid] {
			walk(c)
		}
	}
	for r := range m.roots {
		walk(r)
	}
	if rss > 0 {
		m.peakRSS = max(m.peakRSS, rss)
		m.rssSum += float64(rss)
		m.rssN++
	}
}
