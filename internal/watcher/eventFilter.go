package watcher

import (
	"context"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/tree"
	"local-mirror/pkg/utils"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

// 事件在 1 秒静默窗口内合并批量落库；缓存被监听 goroutine 和
// timer 回调 goroutine 同时读写，必须加锁
var (
	eventMu           sync.Mutex
	createEventCache  []*tree.Node
	createTimer       *time.Timer
	createTimerActive bool
	deleteEventCache  []string
	deleteTimer       *time.Timer
	deleteTimerActive bool
)

// 文件哈希按路径防抖：正在被流式写入的大文件会连发 Write 事件，若每个
// 事件都同步全量重哈希，唯一的事件消费 goroutine 会被占满，拖垮整个
// 同步根的变更检测实时性（真实网络测试实测 300MB 写入卡住 50 秒长轮询）。
// 改为：事件只重置该路径的定时器，静默 hashDebounce 后在定时器 goroutine
// 里做一次最终的 stat+哈希+落库，不阻塞事件主循环。
const hashDebounce = 1 * time.Second

var (
	pendingMu     sync.Mutex
	pendingHashes = make(map[string]*time.Timer) // key: 事件的绝对路径
)

func eventFilter(event fsnotify.Event) {
	relPath := utils.RelPath(config.StartPath, event.Name)
	if utils.IsIgnored(relPath, config.IgnoreFileList) {
		return
	}
	nodeDir := filepath.Dir(relPath)
	fatherNode, err := tree.GetNodeByPath(nodeDir)
	if err != nil {
		// 父节点缺失通常是事件竞态的结果（典型：目录被删后 1 秒内重建，
		// 延迟的批量删除把重建的新子树清掉了）。丢弃事件会让服务端树与
		// 磁盘永久脱节——热目录不做磁盘校验，只有重启才能愈合。改为从
		// 最近的已知祖先按磁盘现状重建缺失链；本事件对应的条目会在重建
		// 扫描中重新产生，不会丢失
		log.Warnf("目录树缺失 %s 的父节点，从最近已知祖先重建缺失链", relPath)
		repairMissingChain(nodeDir)
		return
	}

	switch {
	case event.Has(fsnotify.Create) || event.Has(fsnotify.Write):
		// 路径重新出现：撤销针对同一路径的未落库删除，否则删除合并窗口
		// 结束后 DeleteNodes 按路径查到的是重建后的新节点，会连同其子树
		// 一起误删（对称于 Remove 分支撤销 pendingHashes）
		cancelPendingDelete(relPath)
		// 用 Lstat 而非 Stat：先判断是不是符号链接，若是则跳过不追踪
		// （与 buildFileTree 一致，防止解引用泄露链接目标内容）
		linfo, err := os.Lstat(event.Name)
		if err != nil {
			log.Error("Error getting file info:", err)
			return
		}
		if linfo.Mode()&os.ModeSymlink != 0 {
			log.Warnf("跳过符号链接（不支持同步）: %s", relPath)
			return
		}
		if !linfo.IsDir() {
			// 文件：防抖后在定时器 goroutine 里哈希落库，不阻塞事件主循环
			scheduleFileChange(event.Name)
			return
		}
		uuid, _ := utils.RandomString(16)
		newLeaf := &tree.Node{
			ID:       uuid,
			Path:     relPath,
			Name:     filepath.Base(event.Name),
			ParentID: fatherNode.ID,
			IsDir:    true,
			Size:     uint64(linfo.Size()),
			ModTime:  linfo.ModTime(),
			Hash:     "",
			Depth:    strings.Count(relPath, string(filepath.Separator)),
		}
		if event.Has(fsnotify.Create) {
			GlobalScoreWatch.addHeat(newLeaf.Path, newLeaf)
		}
		// 新目录的内容可能在 watch 建立之前就已写入（如 mkdir -p 或整体移入），
		// 这些内容永远不会再有事件；立即落库目录节点并递归扫描其内容
		eventMu.Lock()
		createEventCache = append(createEventCache, newLeaf)
		eventMu.Unlock()
		flushCreateEvents() // 变更日志由 flush 在落库后记录
		scanNewDirContents(event.Name)
	case event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename):
		// Rename 事件表示旧路径消失，等同删除；若产生了新路径会另收到 Create 事件。
		//
		// 但先做磁盘核验：路径仍存在则不是真删除。原子替换（写临时文件后
		// rename 覆盖同名旧文件——vite/electron 等构建工具的产物写法，本
		// 工具自身落盘也如此）在 kqueue 的事件合并批次里可能只留下旧 inode
		// 的 Remove 而没有同名 Create，按删除处理会把磁盘上明明存在的文件
		// 从树里静默删掉（投产首日实锤：24 个跨构建同名产物全部丢树）。
		// 存在即转为内容变更处理
		if linfo, statErr := os.Lstat(event.Name); statErr == nil {
			if linfo.Mode()&os.ModeSymlink != 0 {
				return
			}
			if linfo.IsDir() {
				scanNewDirContents(event.Name)
			} else {
				scheduleFileChange(event.Name)
			}
			return
		}
		// 若该文件还有未触发的哈希防抖，一并取消
		pendingMu.Lock()
		if t, ok := pendingHashes[event.Name]; ok {
			t.Stop()
			delete(pendingHashes, event.Name)
		}
		pendingMu.Unlock()
		GlobalScoreWatch.removeHeat(relPath)

		eventMu.Lock()
		deleteEventCache = append(deleteEventCache, relPath)
		if deleteTimerActive {
			deleteTimer.Stop()
		}
		deleteTimer = time.AfterFunc(1*time.Second, flushDeleteEvents)
		deleteTimerActive = true
		eventMu.Unlock()
		// 变更日志由 flushDeleteEvents 在删除落库后记录
	case event.Has(fsnotify.Chmod):
		// 权限/属性变化：文件可能此前因无读权限而哈希缺失（客户端确定性
		// 跳过这类文件），chmod 修复后必须重算哈希同步才能自动恢复——
		// 复用写事件的防抖流水线。内容未变时重算得到相同哈希，只是一次
		// 幂等 upsert，代价可忽略；目录与符号链接的属性变化无需处理
		linfo, err := os.Lstat(event.Name)
		if err != nil || linfo.IsDir() || linfo.Mode()&os.ModeSymlink != 0 {
			return
		}
		scheduleFileChange(event.Name)
	}
}

// unreadableRecheckInterval 不可读文件的恢复探测周期。
// 不能依赖事件：macOS kqueue 对无读权限的文件建不起 watch（需要 open），
// 权限修复不产生任何事件；冷目录轮询只比 size+mtime，chmod 两者都不变。
// 唯一可靠的自愈路径就是对登记过的不可读文件做低频 open 探测
const unreadableRecheckInterval = 30 * time.Second

// recoverUnreadable 定期探测登记的不可读文件，恢复可读即重新入队哈希
// （复用防抖流水线，finalize 成功后自动从登记表移除）。
// 登记表只在出现过读取失败时才有条目，空表时本循环仅一次锁开销
func recoverUnreadable(ctx context.Context) {
	ticker := time.NewTicker(unreadableRecheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, p := range tree.UnreadableSnapshot() {
				f, err := os.Open(p)
				if err != nil {
					if os.IsNotExist(err) {
						// 文件已被删除，Remove 事件/扫描会处理树节点，登记表里不再留守
						tree.UnmarkUnreadable(p)
					}
					continue
				}
				f.Close()
				log.Infof("检测到 %s 已恢复可读，重新计算哈希并恢复同步", p)
				scheduleFileChange(p)
			}
		}
	}
}

// scheduleFileChange 为文件变更安排一次防抖后的哈希落库。
// 同一文件的连续事件只会不断顺延定时器，静默 hashDebounce 后才真正处理。
func scheduleFileChange(absPath string) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if t, ok := pendingHashes[absPath]; ok {
		t.Stop()
	}
	pendingHashes[absPath] = time.AfterFunc(hashDebounce, func() {
		pendingMu.Lock()
		delete(pendingHashes, absPath)
		pendingMu.Unlock()
		finalizeFileChange(absPath)
	})
}

// finalizeFileChange 在防抖静默期结束后运行（定时器 goroutine，不占事件
// 主循环），对文件做最终的一次 stat+哈希并落库。所有状态现查现取：防抖
// 期间文件可能已被删除、被替换为符号链接、或父目录已变。哈希期间文件若
// 继续增长，随之而来的 Write 事件会再排一轮防抖，最终收敛到写入完成后的
// 内容；服务端响应文件请求时还会现算哈希，这里的值过期不影响数据正确性。
func finalizeFileChange(absPath string) {
	relPath := utils.RelPath(config.StartPath, absPath)
	linfo, err := os.Lstat(absPath)
	if err != nil {
		return // 防抖期间已删除，Remove 事件自会处理
	}
	if linfo.Mode()&os.ModeSymlink != 0 || linfo.IsDir() {
		return
	}
	fatherNode, err := tree.GetNodeByPath(filepath.Dir(relPath))
	if err != nil {
		// 与 eventFilter 同源的竞态：父目录节点缺失即重建缺失链。
		// 重建扫描会重新调度本文件的哈希，此处直接返回不丢数据
		log.Warnf("目录树缺失 %s 的父节点，从最近已知祖先重建缺失链", relPath)
		repairMissingChain(filepath.Dir(relPath))
		return
	}
	hash := ""
	if h, hashErr := utils.CalcBlake3(absPath); hashErr != nil {
		// 与 buildFileTree 语义一致：空哈希节点照常落库（客户端确定性跳过、
		// 不误删镜像侧副本），登记进不可读列表由恢复循环定期探测
		tree.MarkUnreadable(absPath)
		log.Errorf("无法读取 %s（%v），该文件暂不参与同步；修复权限后会自动恢复", absPath, hashErr)
	} else {
		hash = fmt.Sprintf("%x", h)
		tree.UnmarkUnreadable(absPath)
	}
	uuid, _ := utils.RandomString(16)
	newLeaf := &tree.Node{
		ID:       uuid,
		Path:     relPath,
		Name:     filepath.Base(absPath),
		ParentID: fatherNode.ID,
		IsDir:    false,
		Size:     uint64(linfo.Size()),
		ModTime:  linfo.ModTime(),
		Hash:     hash,
		Depth:    strings.Count(relPath, string(filepath.Separator)),
	}

	eventMu.Lock()
	createEventCache = append(createEventCache, newLeaf)
	if createTimerActive {
		createTimer.Stop()
	}
	createTimer = time.AfterFunc(1*time.Second, flushCreateEvents)
	createTimerActive = true
	eventMu.Unlock()
	// 变更日志由 flushCreateEvents 在节点落库后记录（先记后写会丢变更）
}

// repairMissingChain 当事件的父目录节点不在树中时，从最近的已存在祖先
// 开始按磁盘现状重建缺失的目录链：对链上第一个缺失的目录合成 Create
// 事件，目录分支落库后会递归扫描其内容，缺失的整条链（含触发本次修复
// 的那个条目）由此重建。只触及缺失链本身，不波及已一致的兄弟子树
func repairMissingChain(relDir string) {
	missing := relDir
	for {
		parent := filepath.Dir(missing)
		if parent == missing {
			return // 防御：不应到达
		}
		if parent == "." {
			break // 根目录必在树中
		}
		if ok, err := tree.HasPath(parent); err == nil && ok {
			break
		}
		missing = parent
	}
	absMissing := filepath.Join(config.StartPath, missing)
	if info, err := os.Lstat(absMissing); err != nil || !info.IsDir() {
		return // 磁盘上也已不存在（纯删除场景），无需重建
	}
	eventFilter(fsnotify.Event{Name: absMissing, Op: fsnotify.Create})
}

// cancelPendingDelete 撤销尚未落库的删除。路径在删除合并窗口内重新出现
// （构建工具删目录再快速重建是常态）时必须撤销，否则窗口结束后的批量
// 删除按路径解析到的是重建后的新节点，会连同新子树一起误删
func cancelPendingDelete(relPath string) {
	eventMu.Lock()
	defer eventMu.Unlock()
	if len(deleteEventCache) == 0 {
		return
	}
	kept := deleteEventCache[:0]
	for _, p := range deleteEventCache {
		if p != relPath {
			kept = append(kept, p)
		}
	}
	deleteEventCache = kept
}

// scanNewDirContents 对新出现的目录做一次浅层扫描，
// 为每个条目合成 Create 事件；子目录在 eventFilter 中递归处理
func scanNewDirContents(fullPath string) {
	entries, err := os.ReadDir(fullPath)
	if err != nil {
		log.Warnf("Failed to scan new directory %s: %v", fullPath, err)
		return
	}
	for _, entry := range entries {
		eventFilter(fsnotify.Event{
			Name: filepath.Join(fullPath, entry.Name()),
			Op:   fsnotify.Create,
		})
	}
}

func flushCreateEvents() {
	eventMu.Lock()
	batch := createEventCache
	createEventCache = nil
	createTimerActive = false
	eventMu.Unlock()

	if len(batch) == 0 {
		return
	}
	if err := tree.AddNodes(batch); err != nil {
		log.Errorf("Failed to add nodes: %v", err)
		return
	}
	log.Debugf("Added nodes count %d", len(batch))
	// 变更日志必须在节点落库之后记录，且由落库方记录：若先记日志后落库
	//（此前 finalizeFileChange 的顺序），客户端会在间隙里醒来读到旧树，
	// 而落库本身不再产生新的日志条目，这次变更就静默丢失到下次全量扫描
	parents := make(map[string]struct{}, len(batch))
	for _, n := range batch {
		parents[filepath.Dir(n.Path)] = struct{}{}
	}
	for p := range parents {
		tree.AddRecentChangedDir(p)
	}
}

func flushDeleteEvents() {
	eventMu.Lock()
	batch := deleteEventCache
	deleteEventCache = nil
	deleteTimerActive = false
	eventMu.Unlock()

	if len(batch) == 0 {
		return
	}
	// 落库前的最后一道磁盘核验：合并窗口内路径可能已重新出现（原子替换
	// 的陈旧 Remove、删后快速重建），仍存在的路径不删，转为重哈希/重扫。
	// 与事件时的核验互为冗余——两次检查之间的竞态窗口由此关闭
	kept := batch[:0]
	for _, p := range batch {
		abs := filepath.Join(config.StartPath, p)
		linfo, err := os.Lstat(abs)
		if err != nil || linfo.Mode()&os.ModeSymlink != 0 {
			kept = append(kept, p) // 确实没了（或是不追踪的符号链接残留节点），照删
			continue
		}
		if linfo.IsDir() {
			scanNewDirContents(abs)
		} else {
			scheduleFileChange(abs)
		}
	}
	batch = kept
	if len(batch) == 0 {
		return
	}
	if err := tree.DeleteNodes(batch); err != nil {
		log.Errorf("Failed to delete nodes: %v", err)
		return
	}
	log.Debugf("Deleted nodes count %d", len(batch))
	// 同 flushCreateEvents：日志跟着落库走，删除对下游可见后才通知
	parents := make(map[string]struct{}, len(batch))
	for _, p := range batch {
		parents[filepath.Dir(p)] = struct{}{}
	}
	for p := range parents {
		tree.AddRecentChangedDir(p)
	}
}
