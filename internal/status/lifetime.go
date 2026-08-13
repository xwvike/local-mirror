package status

import (
	"encoding/json"
	"os"
	"time"
)

// lifeSchema stats.json 的结构版本，读端据此容错跨版本字段变化
const lifeSchema = 1

// lifeFlushEveryBytes 每累计传输这么多字节回写一次 stats.json。以「传输量」而非
// 「文件数」为阈值：文件大小悬殊时（一个 10G 大文件 vs 上万个小文件）都能把硬杀
// （SIGKILL/断电）丢失的计数漂移封顶在这个量级，不会因大文件而落后一大截。
// 回写只在文件传完的边界（RecordFile）判定，绝不在文件传输中途切一刀——大文件
// 在途时压根不碰计数，传完才累加，故不会出现"半个文件的计数"。
// 显示值走内存、始终精确；本阈值只影响硬杀后从盘恢复的精度。
// 优雅退出再由 main 显式刷一次（FlushLifetime），故正常路径零丢失
const lifeFlushEveryBytes = 1 << 20 // 1 MiB

var (
	// lifePath <同步根>/.local-mirror/stats.json：终身累计的持久化落点
	lifePath string
	// lifeFlushedBytes 上次回写 stats.json 时的 LifetimeBytes，用于传输量阈值判定
	lifeFlushedBytes uint64
)

// lifetimeState stats.json 的磁盘结构。与 status.json 不同，它不随观测门开关、
// 也不随重启清零——是「该同步根自开始工作以来」的传输总量真凭。
// 与 .local-mirror 其余状态一样可弃：删了下次启动即从此刻重新计数（只是丢历史）
type lifetimeState struct {
	Schema      int    `json:"schema"`
	SinceUnix   int64  `json:"since_unix"`   // 首次开始计数的时刻
	Files       uint64 `json:"files"`        // 终身累计传输文件数
	Bytes       uint64 `json:"bytes"`        // 终身累计传输字节数
	UpdatedUnix int64  `json:"updated_unix"` // 本次回写时刻
}

// loadLifetimeFile 读取 stats.json。缺失/损坏都返回 nil（调用方据此从此刻起算），
// 不让一个坏计数文件挡住启动
func loadLifetimeFile(p string) *lifetimeState {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var ls lifetimeState
	if err := json.Unmarshal(data, &ls); err != nil {
		return nil
	}
	return &ls
}

// FlushLifetime 把终身累计原子写入 stats.json。进程优雅退出时由 main 显式调用一次，
// 确保关机前把最后不足一个批量的增量落盘。可安全在退出路径同步调用
func FlushLifetime() {
	flushLifetime()
}

// flushLifetime 快照终身计数（持锁）后在锁外原子落盘，避免写盘 I/O 占着 mu。
// 临时文件 + rename，读端不会读到半个 JSON
func flushLifetime() {
	mu.Lock()
	if !enabled || lifePath == "" {
		mu.Unlock()
		return
	}
	ls := lifetimeState{
		Schema:      lifeSchema,
		SinceUnix:   snap.LifetimeSinceUnix,
		Files:       snap.LifetimeFiles,
		Bytes:       snap.LifetimeBytes,
		UpdatedUnix: time.Now().Unix(),
	}
	p := lifePath
	lifeFlushedBytes = snap.LifetimeBytes
	mu.Unlock()

	data, err := json.MarshalIndent(&ls, "", "  ")
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}
