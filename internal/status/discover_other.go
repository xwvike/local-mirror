//go:build !linux && !darwin

package status

// discoverProcRoots 其他平台（windows 等）暂不支持进程表发现：
// --status --all 会得到空列表，单实例 --status -p/cwd 仍可用
func discoverProcRoots() []procRoot {
	return nil
}
