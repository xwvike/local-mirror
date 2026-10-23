//go:build !darwin

package tree

import "runtime"

// hashWorkers 建树时并发计算文件哈希的 worker 数。Linux（ext4）上并发 open 没有
// 明显竞争，按 CPU 数开满
func hashWorkers() int { return runtime.NumCPU() }
