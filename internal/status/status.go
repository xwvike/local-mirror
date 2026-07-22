// Package status 维护一份运行时状态快照并周期性写入
// <同步根>/.local-mirror/status.json（公网化运维特性，见
// docs/PUBLIC_EXPOSURE.md 与 v1.0 后方向讨论）。
//
// 设计取「快照文件」而非控制 socket：常驻进程每 flushInterval 秒、以及每次
// 状态变化时原子写盘；`local-mirror --status` 是**另一个进程**，只读这份文件
// 并渲染，顺带用 updated_unix 的新旧判断常驻进程是否还活着——进程崩了也能看到
// 最后已知状态 + 陈旧告警，且完全不碰同步协议、跨平台一致、自包含于状态目录。
//
// 与 .local-mirror/ 里 cache.db / logs / partial 一样属可弃状态：删了下次启动
// 自动重建，不影响同步。
package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// SchemaVersion status.json 的结构版本，读端据此容错跨版本字段变化。
// v2：新增进行中传输（current_*）、速率、自采资源（cpu/rss/fd/heap）
const SchemaVersion = 2

// idleInterval/activeInterval 落盘节奏：连接活跃时 1s（供 --status 实时刷新
// 看到速率/进度/资源），空闲时 5s。读端以 3×idleInterval 为陈旧判据
const (
	idleInterval   = 5 * time.Second
	activeInterval = 1 * time.Second
	// rateWindow 传输速率的滚动窗口
	rateWindow = 5 * time.Second
)

// Snapshot 写入 status.json 的运行时快照。identity 段启动时定型，
// 其余字段随同步进展更新
type Snapshot struct {
	Schema   int    `json:"schema"`
	Version  string `json:"version"`  // 二进制版本
	PID      int    `json:"pid"`      // 常驻进程 PID（读端据此佐证存活）
	Instance string `json:"instance"` // 实例 ID（8 位十六进制）
	Root     string `json:"root"`     // 同步根

	Direction string `json:"direction"` // "send · source" / "receive · sink" / "relay"
	Transport string `json:"transport"` // "listen" / "dial"
	Peer      string `json:"peer"`      // 对端地址（拨出）或 "inbound"（监听）
	Encrypted bool   `json:"encrypted"`

	StartedUnix int64 `json:"started_unix"`

	// 动态段
	Peers        int    `json:"peers"`          // 活跃连接数（源可扇出多个下游；汇恒 0/1）
	Connected    bool   `json:"connected"`      // Peers > 0
	Detail       string `json:"detail"`         // 人读的连接细节
	LastSyncUnix int64  `json:"last_sync_unix"` // 最近一次文件传输完成时刻
	LastFile     string `json:"last_file"`      // 最近传输完成的文件（相对路径）
	Files        uint64 `json:"files"`          // 累计传输文件数
	Bytes        uint64 `json:"bytes"`          // 累计传输字节数
	Errors       uint64 `json:"errors"`         // 累计连接级错误数

	// 进行中的传输。收方下载严格串行（协议单飞行），故精确；
	// 发方扇出多下游时为最后写入者（展示近似，不影响累计计数）
	CurrentFile  string  `json:"current_file"`
	CurrentDone  uint64  `json:"current_done"`
	CurrentTotal uint64  `json:"current_total"`
	RateBps      float64 `json:"rate_bps"` // 滚动传输速率（字节/秒）

	// 自采资源占用（常驻进程测自己，读端只显示，保证跨平台）
	CPUPercent float64 `json:"cpu_percent"`
	RSSBytes   uint64  `json:"rss_bytes"` // 常驻集（linux 精确当前值；darwin 为峰值近似）
	HasRSS     bool    `json:"has_rss"`
	HeapBytes  uint64  `json:"heap_bytes"` // Go 存活堆
	SysBytes   uint64  `json:"sys_bytes"`  // Go 向 OS 申请的总量
	Goroutines int     `json:"goroutines"`
	FDs        int     `json:"fds"` // 打开的文件描述符数（linux 精确；其他平台 -1=未知）
	HasFDs     bool    `json:"has_fds"`

	UpdatedUnix int64 `json:"updated_unix"` // 本快照写盘时刻（陈旧判据）
}

// rateSample 累计已传字节在某时刻的取样，用于滚动速率
type rateSample struct {
	t   time.Time
	cum uint64
}

// procStats 平台相关的进程自采资源（见 process_*.go）
type procStats struct {
	CPUSeconds float64 // 累计 CPU 时间（用户+内核），跨采样求差得占用率
	RSS        uint64
	HasRSS     bool
	FDs        int
	HasFDs     bool
}

var (
	mu      sync.Mutex
	snap    Snapshot
	path    string
	enabled bool
	// poke 合并触发即时落盘：状态变化时非阻塞投递，写循环随即刷一次，
	// 避免高频变化下每次都同步写盘
	poke = make(chan struct{}, 1)

	rateSamples  []rateSample
	lastSampleAt time.Time

	prevCPUSecs float64
	prevCPUAt   time.Time
	cpuPrimed   bool
)

// Init 定型 identity 段并启用快照。须在进程确定方向/加密/端口之后调用一次
func Init(root, version, instance, direction, transport, peer string, encrypted bool, started int64) {
	mu.Lock()
	defer mu.Unlock()
	snap = Snapshot{
		Schema:      SchemaVersion,
		Version:     version,
		PID:         os.Getpid(),
		Instance:    instance,
		Root:        root,
		Direction:   direction,
		Transport:   transport,
		Peer:        peer,
		Encrypted:   encrypted,
		StartedUnix: started,
	}
	path = filepath.Join(root, ".local-mirror", "status.json")
	enabled = true
}

// Run 启动落盘循环：连接活跃时 activeInterval、空闲时 idleInterval，外加每次
// poke 即刷，阻塞至 stop 关闭。由常驻进程在后台 goroutine 里跑
func Run(stop <-chan struct{}) {
	if !enabled {
		return
	}
	write() // 启动即落一版，--status 立刻有据可读
	for {
		mu.Lock()
		interval := idleInterval
		if snap.Connected || snap.CurrentFile != "" {
			interval = activeInterval
		}
		mu.Unlock()
		select {
		case <-stop:
			write()
			return
		case <-poke:
			write()
		case <-time.After(interval):
			write()
		}
	}
}

func signal() {
	select {
	case poke <- struct{}{}:
	default:
	}
}

// SessionUp 一条连接就绪（源侧 accept/拨出成功、汇侧握手成功）。
// detail 是人读的对端描述
func SessionUp(detail string) {
	mu.Lock()
	snap.Peers++
	snap.Connected = true
	snap.Detail = detail
	mu.Unlock()
	signal()
}

// SessionDown 一条连接结束
func SessionDown() {
	mu.Lock()
	if snap.Peers > 0 {
		snap.Peers--
	}
	snap.Connected = snap.Peers > 0
	if !snap.Connected {
		snap.Detail = ""
	}
	mu.Unlock()
	signal()
}

// RecordProgress 进行中传输的进度上报（收方下载/发方发送循环里节流调用）。
// 只更新内存态与速率取样，不 poke——落盘由 Run 的活跃节奏（1s）承担，
// 避免每个数据块都写盘
func RecordProgress(file string, done, total uint64) {
	mu.Lock()
	snap.CurrentFile = file
	snap.CurrentDone = done
	snap.CurrentTotal = total
	now := time.Now()
	if now.Sub(lastSampleAt) >= 200*time.Millisecond {
		addRateSampleLocked(now, snap.Bytes+done)
		lastSampleAt = now
	}
	mu.Unlock()
}

// RecordFile 一个文件传输完成（收方下载完 / 发方发完）
func RecordFile(relPath string, n uint64) {
	mu.Lock()
	snap.Files++
	snap.Bytes += n
	snap.LastFile = relPath
	snap.LastSyncUnix = time.Now().Unix()
	snap.CurrentFile = ""
	snap.CurrentDone = 0
	snap.CurrentTotal = 0
	addRateSampleLocked(time.Now(), snap.Bytes)
	mu.Unlock()
	signal()
}

// addRateSampleLocked 追加一个累计字节取样（调用方须持锁）
func addRateSampleLocked(t time.Time, cum uint64) {
	rateSamples = append(rateSamples, rateSample{t: t, cum: cum})
}

// computeRateLocked 依滚动窗口算速率并顺带修剪过期取样（调用方须持锁）。
// 传输停止后窗口内取样耗尽（<2 个）→ 速率归 0
func computeRateLocked(now time.Time) float64 {
	cut := now.Add(-rateWindow)
	i := 0
	for i < len(rateSamples) && rateSamples[i].t.Before(cut) {
		i++
	}
	rateSamples = rateSamples[i:]
	if len(rateSamples) < 2 {
		return 0
	}
	first, last := rateSamples[0], rateSamples[len(rateSamples)-1]
	dt := last.t.Sub(first.t).Seconds()
	if dt <= 0 || last.cum < first.cum {
		return 0
	}
	return float64(last.cum-first.cum) / dt
}

// sampleResourcesLocked 采集本进程资源占用（调用方须持锁）。
// CPU 占用率由两次采样的累计 CPU 时间差 / 墙钟差得出
func sampleResourcesLocked(now time.Time) {
	ps := sampleProc()
	if cpuPrimed {
		if dw := now.Sub(prevCPUAt).Seconds(); dw > 0 {
			pct := 100 * (ps.CPUSeconds - prevCPUSecs) / dw
			if pct < 0 {
				pct = 0
			}
			snap.CPUPercent = pct
		}
	}
	prevCPUSecs = ps.CPUSeconds
	prevCPUAt = now
	cpuPrimed = true

	snap.RSSBytes = ps.RSS
	snap.HasRSS = ps.HasRSS
	snap.FDs = ps.FDs
	snap.HasFDs = ps.HasFDs

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	snap.HeapBytes = m.HeapAlloc
	snap.SysBytes = m.Sys
	snap.Goroutines = runtime.NumGoroutine()
}

// RecordError 一次连接级错误（掉线、握手失败、传输中断等）
func RecordError() {
	mu.Lock()
	snap.Errors++
	mu.Unlock()
	// 错误不即时 poke：错误常伴随重连风暴，交给周期刷即可，避免写盘抖动
}

// write 原子落盘：同目录临时文件 + rename，避免读端读到半个 JSON。
// 落盘前顺带刷新速率与资源采样（每次落盘节奏即采样节奏）
func write() {
	mu.Lock()
	if !enabled {
		mu.Unlock()
		return
	}
	now := time.Now()
	snap.RateBps = computeRateLocked(now)
	sampleResourcesLocked(now)
	snap.UpdatedUnix = now.Unix()
	data, err := json.MarshalIndent(&snap, "", "  ")
	p := path
	mu.Unlock()
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

// Load 读取并解析 status.json（供 --status 子命令）。
// 文件不存在返回 (nil, nil)：调用方据此报「无运行实例」
func Load(root string) (*Snapshot, error) {
	data, err := os.ReadFile(filepath.Join(root, ".local-mirror", "status.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Stale 快照是否已陈旧（超过 3×idleInterval 未更新 = 常驻进程可能已停）
func (s *Snapshot) Stale() bool {
	return s.Age() > 3*idleInterval
}

// Age 距上次写盘的时长
func (s *Snapshot) Age() time.Duration {
	return time.Since(time.Unix(s.UpdatedUnix, 0))
}
