package watcher

import (
	"errors"
	"fmt"
	"local-mirror/pkg/utils"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

// fsWatcher 两级监控对文件系统事件源的全部要求，语义与 fsnotify.Watcher 一致：
// Add/Remove 针对单个目录、不递归，事件报告该目录的直接子项。平台有更好的
// 原生机制时（darwin 的 FSEvents）换掉实现，上层不感知
type fsWatcher interface {
	Add(path string) error
	Remove(path string) error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	Close() error
	// WatchCost 给一个有 entries 个条目的目录建实时 watch 要占多少描述符单位，
	// 两级监控据此分配预算。为 0 表示 watch 不占描述符，所有目录都能实时监听
	WatchCost(entries int) int
}

var errNoNativeWatcher = errors.New("no native watcher on this platform")

// rescanError 事件源丢了事件（内核或守护进程的队列溢出）：Path 下的变化可能
// 没报全，要按磁盘重新核对。Path 为空表示整个同步根。errors.Is 匹配
// fsnotify.ErrEventOverflow，与 inotify 队列溢出走同一条处理路径
type rescanError struct {
	Path string
}

func (e *rescanError) Error() string {
	if e.Path == "" {
		return "file events were dropped, the whole tree needs a rescan"
	}
	return fmt.Sprintf("file events were dropped under %s, it needs a rescan", e.Path)
}

func (e *rescanError) Unwrap() error { return fsnotify.ErrEventOverflow }

// NewFSWatcher 为同步根 root 建立事件源：平台有原生实现就用它，建不起来退回 fsnotify
func NewFSWatcher(root string) (fsWatcher, error) {
	w, err := newNativeWatcher(root)
	if err == nil {
		return w, nil
	}
	if err != errNoNativeWatcher {
		log.Warnf("watcher: native file events unavailable (%v), falling back to fsnotify", err)
	}
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return wrapFsnotify(fw), nil
}

// fsnotifyWatcher 把 fsnotify.Watcher 适配成 fsWatcher
type fsnotifyWatcher struct {
	w *fsnotify.Watcher
}

func wrapFsnotify(w *fsnotify.Watcher) fsWatcher { return fsnotifyWatcher{w} }

func (f fsnotifyWatcher) Add(path string) error         { return f.w.Add(path) }
func (f fsnotifyWatcher) Remove(path string) error      { return f.w.Remove(path) }
func (f fsnotifyWatcher) Events() <-chan fsnotify.Event { return f.w.Events }
func (f fsnotifyWatcher) Errors() <-chan error          { return f.w.Errors }
func (f fsnotifyWatcher) Close() error                  { return f.w.Close() }

// WatchCost darwin 上 fsnotify 走 kqueue，为目录及其每个条目各占一个描述符；
// inotify 的 watch 不占描述符（整个实例共用一个），按 1 计
func (f fsnotifyWatcher) WatchCost(entries int) int {
	if utils.BaseOSInfo().OS == "darwin" {
		return entries + 1
	}
	return 1
}
