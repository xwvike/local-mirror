//go:build !linux && !darwin

package status

// sampleProc 其他平台（windows 等）无 getrusage：资源占用标未知，
// 读端回退到 Go 运行时数字（堆/goroutine，跨平台可得）
func sampleProc() procStats {
	return procStats{FDs: -1}
}
