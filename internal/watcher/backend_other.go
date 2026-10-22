//go:build !darwin

package watcher

// newNativeWatcher inotify（linux）与 ReadDirectoryChangesW（windows）由 fsnotify
// 直接使用，没有更好的原生实现
func newNativeWatcher(string) (fsWatcher, error) { return nil, errNoNativeWatcher }
