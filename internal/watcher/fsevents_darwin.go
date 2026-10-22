//go:build darwin

package watcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/fsnotify/fsnotify"
)

// fseventsWatcher 用 FSEvents 实现 fsWatcher。
//
// fsnotify 在 darwin 上走 kqueue，为每个被监视的目录及其每个条目各开一个描述符，
// 十万文件的同步根铺不开，大部分目录只能降级到分钟级的轮询。FSEvents 由内核
// 与 fseventsd 维护，整个同步根一个事件流，不占描述符，开销只随变化频率增长。
//
// 对外保持 fsnotify 的语义：Add/Remove 针对单个目录、不递归，只转发父目录在
// 监视集合里的事件，于是两级监控的规划逻辑原样适用；WatchCost 恒为 0，所有
// 目录都会进入实时层，轮询层自然为空。
//
// 通过 purego 调用 CoreServices，不引入 cgo，发布仍可交叉编译
type fseventsWatcher struct {
	root     string // 调用方给的同步根（事件路径翻译回这个前缀）
	realRoot string // FSEvents 报告的是解析过符号链接的真实路径

	mu      sync.Mutex
	watched map[string]struct{}

	raw    chan []rawEvent
	events chan fsnotify.Event
	errors chan error
	done   chan struct{}
	closed sync.Once
	exited chan struct{}

	id     uintptr
	queue  uintptr
	stream uintptr
}

type rawEvent struct {
	path  string
	flags uint32
}

// FSEvents 常量，取自 <CoreServices/FSEvents.h>
const (
	fseCreateNoDefer    = 0x00000002
	fseCreateWatchRoot  = 0x00000004
	fseCreateFileEvents = 0x00000010

	fseMustScanSubDirs  = 0x00000001
	fseUserDropped      = 0x00000002
	fseKernelDropped    = 0x00000004
	fseRootChanged      = 0x00000020
	fseItemCreated      = 0x00000100
	fseItemRemoved      = 0x00000200
	fseItemInodeMetaMod = 0x00000400
	fseItemRenamed      = 0x00000800
	fseItemModified     = 0x00001000
	fseItemChangeOwner  = 0x00004000

	fseEventIDSinceNow    = 0xFFFFFFFFFFFFFFFF
	cfStringEncodingUTF8  = 0x08000100
	rtldNowGlobal         = purego.RTLD_NOW | purego.RTLD_GLOBAL
	fseventsLatencySecond = 0.05
)

// fseventsStreamContext 对应 FSEventStreamContext：version 须为 0，其后依次是
// info、retain、release、copyDescription。info 放监视器编号而非 Go 指针，
// 三个回调留空
type fseventsStreamContext struct {
	_    int
	info uintptr
	_    [3]uintptr
}

var (
	fseOnce    sync.Once
	fseLoadErr error

	cfStringCreateWithCString func(alloc uintptr, s string, encoding uint32) uintptr
	cfArrayCreate             func(alloc uintptr, values *uintptr, n int, callbacks uintptr) uintptr
	cfRelease                 func(ref uintptr)
	fsEventStreamCreate       func(alloc, callback uintptr, ctx *fseventsStreamContext, paths uintptr, since uint64, latency float64, flags uint32) uintptr
	fsEventStreamSetQueue     func(stream, queue uintptr)
	fsEventStreamStart        func(stream uintptr) bool
	fsEventStreamStop         func(stream uintptr)
	fsEventStreamInvalidate   func(stream uintptr)
	fsEventStreamRelease      func(stream uintptr)
	dispatchQueueCreate       func(label string, attr uintptr) uintptr
	dispatchSyncF             func(queue, ctx, work uintptr)
	dispatchRelease           func(obj uintptr)

	cfTypeArrayCallBacks uintptr
	fseCallback          uintptr
	fseNoop              uintptr

	// fseRegistry 回调按 info 里的编号找回监视器。C 侧不持有 Go 指针
	fseRegistry   = map[uintptr]*fseventsWatcher{}
	fseRegistryMu sync.Mutex
	fseNextID     uintptr
)

func loadFSEvents() error {
	fseOnce.Do(func() {
		cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", rtldNowGlobal)
		if err != nil {
			fseLoadErr = err
			return
		}
		cs, err := purego.Dlopen("/System/Library/Frameworks/CoreServices.framework/CoreServices", rtldNowGlobal)
		if err != nil {
			fseLoadErr = err
			return
		}
		libSystem, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", rtldNowGlobal)
		if err != nil {
			fseLoadErr = err
			return
		}
		if cfTypeArrayCallBacks, err = purego.Dlsym(cf, "kCFTypeArrayCallBacks"); err != nil {
			fseLoadErr = err
			return
		}
		purego.RegisterLibFunc(&cfStringCreateWithCString, cf, "CFStringCreateWithCString")
		purego.RegisterLibFunc(&cfArrayCreate, cf, "CFArrayCreate")
		purego.RegisterLibFunc(&cfRelease, cf, "CFRelease")
		purego.RegisterLibFunc(&fsEventStreamCreate, cs, "FSEventStreamCreate")
		purego.RegisterLibFunc(&fsEventStreamSetQueue, cs, "FSEventStreamSetDispatchQueue")
		purego.RegisterLibFunc(&fsEventStreamStart, cs, "FSEventStreamStart")
		purego.RegisterLibFunc(&fsEventStreamStop, cs, "FSEventStreamStop")
		purego.RegisterLibFunc(&fsEventStreamInvalidate, cs, "FSEventStreamInvalidate")
		purego.RegisterLibFunc(&fsEventStreamRelease, cs, "FSEventStreamRelease")
		purego.RegisterLibFunc(&dispatchQueueCreate, libSystem, "dispatch_queue_create")
		purego.RegisterLibFunc(&dispatchSyncF, libSystem, "dispatch_sync_f")
		purego.RegisterLibFunc(&dispatchRelease, libSystem, "dispatch_release")
		// 回调只建一次：purego 的回调永不释放，数量有上限
		fseCallback = purego.NewCallback(fseventsCallback)
		fseNoop = purego.NewCallback(func(unsafe.Pointer) {})
	})
	return fseLoadErr
}

func newNativeWatcher(root string) (fsWatcher, error) {
	if err := loadFSEvents(); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	realRoot, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	w := &fseventsWatcher{
		root:     abs,
		realRoot: realRoot,
		watched:  map[string]struct{}{},
		raw:      make(chan []rawEvent, 64),
		events:   make(chan fsnotify.Event, 1024),
		errors:   make(chan error, 16),
		done:     make(chan struct{}),
		exited:   make(chan struct{}),
	}

	fseRegistryMu.Lock()
	fseNextID++
	w.id = fseNextID
	fseRegistry[w.id] = w
	fseRegistryMu.Unlock()

	cfPath := cfStringCreateWithCString(0, realRoot, cfStringEncodingUTF8)
	if cfPath == 0 {
		w.unregister()
		return nil, errors.New("CFStringCreateWithCString failed")
	}
	paths := cfArrayCreate(0, &cfPath, 1, cfTypeArrayCallBacks)
	cfRelease(cfPath) // 数组已持有
	if paths == 0 {
		w.unregister()
		return nil, errors.New("CFArrayCreate failed")
	}
	ctx := fseventsStreamContext{info: w.id}
	w.stream = fsEventStreamCreate(0, fseCallback, &ctx, paths, fseEventIDSinceNow, fseventsLatencySecond,
		fseCreateFileEvents|fseCreateNoDefer|fseCreateWatchRoot)
	cfRelease(paths)
	if w.stream == 0 {
		w.unregister()
		return nil, errors.New("FSEventStreamCreate failed")
	}
	w.queue = dispatchQueueCreate("local-mirror.fsevents", 0)
	fsEventStreamSetQueue(w.stream, w.queue)
	if !fsEventStreamStart(w.stream) {
		fsEventStreamInvalidate(w.stream)
		fsEventStreamRelease(w.stream)
		dispatchRelease(w.queue)
		w.unregister()
		return nil, errors.New("FSEventStreamStart failed")
	}
	go w.translate()
	return w, nil
}

func (w *fseventsWatcher) unregister() {
	fseRegistryMu.Lock()
	delete(fseRegistry, w.id)
	fseRegistryMu.Unlock()
}

// fseventsCallback 运行在 FSEvents 的派发队列（非 Go 线程）上：只把这一批原样
// 拷出来交给 translate，不在这里 stat，免得拖住队列
func fseventsCallback(stream, info unsafe.Pointer, n uintptr, paths, flags, ids unsafe.Pointer) {
	fseRegistryMu.Lock()
	w := fseRegistry[uintptr(info)]
	fseRegistryMu.Unlock()
	if w == nil || n == 0 {
		return
	}
	cpaths := unsafe.Slice((**byte)(paths), n)
	cflags := unsafe.Slice((*uint32)(flags), n)
	batch := make([]rawEvent, n)
	for i := range batch {
		batch[i] = rawEvent{path: goString(cpaths[i]), flags: cflags[i]}
	}
	select {
	case w.raw <- batch:
	case <-w.done:
	}
}

func goString(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

func (w *fseventsWatcher) translate() {
	defer close(w.exited)
	for {
		select {
		case batch := <-w.raw:
			for _, e := range batch {
				w.handle(e)
			}
		case <-w.done:
			return
		}
	}
}

// handle 把一条 FSEvents 事件翻译成 fsnotify 事件。
//
// FSEvents 的标志位是累积的：合并窗口里同一路径的新建、修改、删除会叠在一条
// 事件上，新建标志还可能残留到之后的修改事件里。所以不按标志推断发生了什么，
// 而是看路径现在的状态：不存在即 Remove；存在则按标志给出 Create/Write/Chmod，
// 下游（eventFilter）对每种操作都会再按磁盘现状核验
func (w *fseventsWatcher) handle(e rawEvent) {
	if e.flags&(fseMustScanSubDirs|fseUserDropped|fseKernelDropped) != 0 {
		path, ok := w.fromReal(e.path)
		if !ok || e.flags&(fseUserDropped|fseKernelDropped) != 0 {
			path = "" // 丢在了队列层面，范围不可知
		}
		w.sendError(&rescanError{Path: path})
		return
	}
	if e.flags&fseRootChanged != 0 {
		w.sendError(fmt.Errorf("sync root %s was moved or deleted", w.root))
		return
	}
	const itemFlags = fseItemCreated | fseItemRemoved | fseItemRenamed | fseItemModified | fseItemInodeMetaMod | fseItemChangeOwner
	if e.flags&itemFlags == 0 {
		return // 只有扩展属性、Finder 信息之类不参与同步的变化
	}
	path, ok := w.fromReal(e.path)
	if !ok || path == w.root {
		return
	}
	// 先按监视集合过滤再 stat：同步根里被忽略的目录（.git、node_modules、
	// 本工具的 .local-mirror）不在集合里，它们的事件在这里就丢掉
	w.mu.Lock()
	_, parentWatched := w.watched[filepath.Dir(path)]
	_, selfWatched := w.watched[path]
	w.mu.Unlock()
	if !parentWatched {
		return
	}

	info, err := os.Lstat(path)
	if err != nil {
		if selfWatched {
			// 与 fsnotify 一致：被监视的目录消失后其 watch 随之失效
			w.mu.Lock()
			delete(w.watched, path)
			w.mu.Unlock()
		}
		w.send(fsnotify.Event{Name: path, Op: fsnotify.Remove})
		return
	}
	var op fsnotify.Op
	switch {
	case info.IsDir() && selfWatched:
		// 已知目录：新建标志多半是残留的，不能当新目录重扫整棵子树。
		// 改名移入（同名替换）才需要重扫
		switch {
		case e.flags&fseItemRenamed != 0:
			op = fsnotify.Create
		case e.flags&(fseItemInodeMetaMod|fseItemChangeOwner) != 0:
			op = fsnotify.Chmod
		default:
			return
		}
	case info.IsDir():
		op = fsnotify.Create
	case e.flags&(fseItemCreated|fseItemRenamed) != 0:
		op = fsnotify.Create
	case e.flags&fseItemModified != 0:
		op = fsnotify.Write
	default:
		op = fsnotify.Chmod
	}
	w.send(fsnotify.Event{Name: path, Op: op})
}

// fromReal 把 FSEvents 报告的真实路径换回调用方的同步根前缀。
// APFS 默认大小写不敏感，用户给的根路径大小写可能与磁盘不同，前缀按不区分大小写比较
func (w *fseventsWatcher) fromReal(p string) (string, bool) {
	n := len(w.realRoot)
	if len(p) < n || !strings.EqualFold(p[:n], w.realRoot) {
		return "", false
	}
	rest := p[n:]
	if rest != "" && rest[0] != '/' {
		return "", false
	}
	return w.root + rest, true
}

func (w *fseventsWatcher) send(e fsnotify.Event) {
	select {
	case w.events <- e:
	case <-w.done:
	}
}

func (w *fseventsWatcher) sendError(err error) {
	select {
	case w.errors <- err:
	case <-w.done:
	}
}

func (w *fseventsWatcher) Add(path string) error {
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	w.mu.Lock()
	w.watched[path] = struct{}{}
	w.mu.Unlock()
	return nil
}

func (w *fseventsWatcher) Remove(path string) error {
	path = filepath.Clean(path)
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.watched[path]; !ok {
		return fmt.Errorf("%w: %s", fsnotify.ErrNonExistentWatch, path)
	}
	delete(w.watched, path)
	return nil
}

func (w *fseventsWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *fseventsWatcher) Errors() <-chan error          { return w.errors }
func (w *fseventsWatcher) WatchCost(int) int             { return 0 }

// Close 停流并等派发队列上在跑的回调退出后才关 channel：先关 done 让阻塞在
// 投递上的回调返回，停流之后在同一个串行队列上同步派发一个空任务作为屏障
func (w *fseventsWatcher) Close() error {
	w.closed.Do(func() {
		close(w.done)
		fsEventStreamStop(w.stream)
		fsEventStreamInvalidate(w.stream)
		dispatchSyncF(w.queue, 0, fseNoop)
		fsEventStreamRelease(w.stream)
		dispatchRelease(w.queue)
		w.unregister()
		<-w.exited
		close(w.events)
		close(w.errors)
	})
	return nil
}
