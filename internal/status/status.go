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
	"sync"
	"time"
)

// SchemaVersion status.json 的结构版本，读端据此容错跨版本字段变化
const SchemaVersion = 1

// flushInterval 周期性落盘间隔。读端以此为陈旧判据的基准
// （age ≤ 3×interval 视为新鲜，见 --status 渲染）
const flushInterval = 5 * time.Second

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

	UpdatedUnix int64 `json:"updated_unix"` // 本快照写盘时刻（陈旧判据）
}

var (
	mu      sync.Mutex
	snap    Snapshot
	path    string
	enabled bool
	// poke 合并触发即时落盘：状态变化时非阻塞投递，写循环随即刷一次，
	// 避免高频变化下每次都同步写盘
	poke = make(chan struct{}, 1)
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

// Run 启动落盘循环：每 flushInterval 一次 + 每次 poke 一次，阻塞至 stop 关闭。
// 由常驻进程在后台 goroutine 里跑
func Run(stop <-chan struct{}) {
	if !enabled {
		return
	}
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	write() // 启动即落一版，--status 立刻有据可读
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			write()
		case <-poke:
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

// RecordFile 一个文件传输完成（收方下载完 / 发方发完）
func RecordFile(relPath string, n uint64) {
	mu.Lock()
	snap.Files++
	snap.Bytes += n
	snap.LastFile = relPath
	snap.LastSyncUnix = time.Now().Unix()
	mu.Unlock()
	signal()
}

// RecordError 一次连接级错误（掉线、握手失败、传输中断等）
func RecordError() {
	mu.Lock()
	snap.Errors++
	mu.Unlock()
	// 错误不即时 poke：错误常伴随重连风暴，交给周期刷即可，避免写盘抖动
}

// write 原子落盘：同目录临时文件 + rename，避免读端读到半个 JSON
func write() {
	mu.Lock()
	if !enabled {
		mu.Unlock()
		return
	}
	snap.UpdatedUnix = time.Now().Unix()
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

// Stale 快照是否已陈旧（超过 3×flushInterval 未更新 = 常驻进程可能已停）
func (s *Snapshot) Stale() bool {
	return s.Age() > 3*flushInterval
}

// Age 距上次写盘的时长
func (s *Snapshot) Age() time.Duration {
	return time.Since(time.Unix(s.UpdatedUnix, 0))
}
