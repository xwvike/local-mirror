package watcher

// Windows 没有 RLIMIT_NOFILE，ReadDirectoryChangesW 也不按目录消耗描述符，
// watch 名额与描述符预算无关：两个探针都报「不可用」，调用方退回固定默认值
func fdLimit() (int, bool) { return 0, false }

func openFDCount() (int, bool) { return 0, false }
