package tree

import "sync"

// 无法读取（哈希失败）文件的登记表，键为绝对路径。
//
// 扫描（BuildFileTree）、watcher 哈希、服务时读取三处的失败都会登记；
// watcher 的恢复循环定期对登记项做 open 探测，恢复可读即重新入队哈希，
// 打通"修复权限后自动恢复同步"的闭环。
//
// 必须主动探测而不能依赖事件：macOS 的 kqueue 对无读权限的文件根本
// 建不起 watch（fsnotify 需要 open 文件），权限修复不会产生任何事件；
// 冷目录轮询只比较 size+mtime，chmod 两者都不改变。
var (
	unreadableMu    sync.Mutex
	unreadablePaths = make(map[string]struct{})
)

func MarkUnreadable(absPath string) {
	unreadableMu.Lock()
	defer unreadableMu.Unlock()
	unreadablePaths[absPath] = struct{}{}
}

func UnmarkUnreadable(absPath string) {
	unreadableMu.Lock()
	defer unreadableMu.Unlock()
	delete(unreadablePaths, absPath)
}

// UnreadableSnapshot 返回当前登记的全部路径副本
func UnreadableSnapshot() []string {
	unreadableMu.Lock()
	defer unreadableMu.Unlock()
	out := make([]string, 0, len(unreadablePaths))
	for p := range unreadablePaths {
		out = append(out, p)
	}
	return out
}
