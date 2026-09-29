package tree

import (
	"path/filepath"
	"strings"
	"sync"
)

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
	// unreadableDirs 建树时列不出内容的目录（相对路径）。它们的子树沿用缓存节点、不当作
	// 已删除剪掉；文件服务对其及子孙的目录列表回 PermissionDenied，汇端据此整棵跳过，
	// 而不是把"列不出"当成"空目录"删光镜像副本
	unreadableDirs = make(map[string]struct{})
)

func MarkUnreadableDir(rel string) {
	unreadableMu.Lock()
	defer unreadableMu.Unlock()
	unreadableDirs[rel] = struct{}{}
}

func UnmarkUnreadableDir(rel string) {
	unreadableMu.Lock()
	defer unreadableMu.Unlock()
	delete(unreadableDirs, rel)
}

// ResetUnreadableDirs 由 BuildFileTree 在每次全量遍历开始时调用，按本轮磁盘现状重新登记
func ResetUnreadableDirs() {
	unreadableMu.Lock()
	defer unreadableMu.Unlock()
	unreadableDirs = make(map[string]struct{})
}

func UnreadableDirsSnapshot() []string {
	unreadableMu.Lock()
	defer unreadableMu.Unlock()
	out := make([]string, 0, len(unreadableDirs))
	for d := range unreadableDirs {
		out = append(out, d)
	}
	return out
}

// UnderUnreadableDir rel 是否为某个登记目录本身或其子孙
func UnderUnreadableDir(rel string) bool {
	unreadableMu.Lock()
	defer unreadableMu.Unlock()
	for d := range unreadableDirs {
		if d == "." || rel == d || strings.HasPrefix(rel, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

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
