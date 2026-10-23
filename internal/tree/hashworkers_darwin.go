package tree

import "runtime"

// hashWorkers 建树时并发计算文件哈希的 worker 数。macOS 上同时 open 的线程一多，
// 内核里的锁竞争会让每次 open 的 CPU 成倍上涨，耗时反而变长：Apple M4（4 性能核
// + 6 能效核）上对 10 万个小文件 open+哈希，4 个 worker 用 4.5 秒 CPU、0.96 秒，
// 10 个 worker 用 22 秒 CPU、2.4 秒。超过 4 个就开始变差，故封顶 4
func hashWorkers() int { return min(runtime.NumCPU(), 4) }
