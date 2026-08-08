package app

import (
	"errors"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/internal/network"
	"local-mirror/internal/status"
	"local-mirror/internal/tree"
	"local-mirror/pkg/stack"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// NextLevel 存放待下钻的目录，由 drainNextLevel 消费
var NextLevel = stack.NewStack[DiffResult]()

var taskMutex sync.Mutex // 确保任务串行执行

// lastChangeCursor 记录变更查询已覆盖到的服务端时刻（unix 秒）。
// 该值始终由服务端返回的 CoveredUntil 推进，绝不使用客户端本地时钟，
// 以免客户端时钟快于服务端时漏查中间窗口的变更（服务端 changed_dirs
// 只保留 1 小时）。0 表示"从窗口起点全查"，用作重连/全量扫描后的重置。
// 任务由 taskMutex 保证串行，无需原子操作。
var lastChangeCursor int64

// handleConnectionError wraps connection error handling to reduce duplication
func handleConnectionError(err error, fileClient *network.FileClient) error {
	if errors.Is(err, appError.ErrConnection) {
		fileClient.ConnectionClose()
	}
	return err
}

func executeTaskWithClient(taskName string, fileClient *network.FileClient, taskFunc func(*network.FileClient) error) error {
	if fileClient.State == network.Deprecated {
		return fmt.Errorf("client is deprecated")
	}

	taskMutex.Lock()
	defer taskMutex.Unlock()

	log.Infof("task started: %s", taskName)
	startTime := time.Now()

	err := taskFunc(fileClient)
	duration := time.Since(startTime)
	if err != nil {
		log.Errorf("task failed %s after %v: %v", taskName, duration, err)
		if errors.Is(err, appError.ErrConnection) {
			return fmt.Errorf("client became deprecated during task: %w", err)
		}
		// §5.1：非连接类错误此前被吞成 nil，会把 DB/初始扫描等真实的任务级失败在上层当成
		// 成功。taskFunc 内部已消化「可跳过的单项/单目录错误」（getDirectory 循环逐项
		// log+continue），能冒到这里的是任务级失败——如实上抛并计入错误统计
		status.RecordError()
		return err
	}
	log.Infof("task done: %s, took %v", taskName, duration)
	return nil
}

// ensureConnected makes sure we have a valid connection
func ensureConnected() (*network.FileClient, error) {
	fileClient, err := InitConn()
	if err != nil {
		fileClient.ConnectionClose()
		// 保留探测的具体失败原因（如加密口令不一致），方便用户定位
		return fileClient, err
	}

	if fileClient.State == network.Online {
		return fileClient, nil
	}

	return fileClient, fmt.Errorf("failed to establish connection")
}

func Mirror() {
	log.Debug("step 3 >> start file client")
	baseDelay := 5 * time.Second
	maxDelay := 60 * time.Second
	currentDelay := baseDelay
	for {
		fileClient, err := ensureConnected()
		if err != nil {
			log.Error("Failed to connect: ", err)
			time.Sleep(currentDelay)
			currentDelay = time.Duration(float64(currentDelay) * 1.5)
			currentDelay = min(currentDelay, maxDelay)
			continue
		}
		currentDelay = baseDelay
		status.SessionUp(fmt.Sprintf("connected to %s", fileClient.RealityAddr))
		err = runMirrorTasks(fileClient)
		status.SessionDown()
		if err != nil {
			status.RecordError()
			log.Errorf("Error running mirror tasks: %v", err)
			fileClient.ConnectionClose()
			time.Sleep(5 * time.Second)
			continue
		}
	}
}

// MirrorListen 汇监听格（--receive --listen，四象限）：不拨出，在
// ServerListener 上等源端拨入，每条入站连接跑一轮完整镜像会话。
// 协议报文与谁拨号无关——汇仍先说话（accept 后立即发握手）。
// 单上游串行服务：会话期间不 accept，多余拨入留在内核 backlog，
// 当前会话结束后自然轮到（拨号方有自己的重试退避）。
// 入站连接断开后不可重拨（主动权在源端），回到 accept 等下一条
func MirrorListen() {
	log.Debug("step 3 >> start sink listener")
	if ServerListener == nil {
		log.Fatal("server listener not initialized")
	}
	log.Infof("Sink listening on %s, waiting for the source to dial in", ServerListener.Addr())
	for {
		conn, err := ServerListener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Error("Error accepting inbound source:", err)
			continue
		}
		prepared, err := network.PrepareInboundConn(conn)
		if err != nil {
			log.Warnf("Rejecting inbound %s: %v", conn.RemoteAddr(), err)
			continue
		}
		fileClient := network.NewFileClientFromConn(prepared)
		if err := fileClient.Handshake(); err != nil {
			log.Warnf("Inbound source %s handshake failed: %v", conn.RemoteAddr(), err)
			fileClient.ConnectionClose()
			continue
		}
		log.Infof("Source dialed in from %s, mirror session starting", conn.RemoteAddr())
		status.SessionUp(fmt.Sprintf("source dialed in from %s", conn.RemoteAddr()))
		if err := runMirrorTasks(fileClient); err != nil {
			status.RecordError()
			log.Errorf("Mirror session over inbound transport ended: %v", err)
		}
		status.SessionDown()
		fileClient.ConnectionClose()
	}
}

// sleepDetectThreshold 长轮询往返最长约 LongPollReadTimeout（60s），
// 墙钟跳变远超此值即判定刚从系统休眠中醒来
const sleepDetectThreshold = 3 * time.Minute

// localRebuildMinInterval 纯汇端从磁盘重建本地树的最小间隔（§5.3）。COR-01 的重建走
// BuildFileTree 全量遍历（O(N) 时间 + 瞬时内存），若每次全量扫描都做（cooldown 可低至
// 几十秒），大树会周期性吃 CPU/内存。故加节流：距上次重建不足此值就跳过——除非
// driftSuspected 置位（如休眠唤醒）时强制重建、无视节流
const localRebuildMinInterval = 10 * time.Minute

var (
	lastLocalRebuild time.Time   // 上次从磁盘重建本地树的时刻（仅纯汇端）
	driftSuspected   atomic.Bool // 置位表示本地可能已漂移，下次全量扫描强制重建
)

// SuspectLocalDrift 标记「本地磁盘可能已漂移」，令下一次全量扫描无视节流、立即从磁盘
// 重建本地树（§5.3 补充触发）。目前由休眠唤醒调用——睡眠期间本地可能被外部改动；
// 将来其它漂移信号（如落盘时发现磁盘状态与树不符）可复用本入口
func SuspectLocalDrift() { driftSuspected.Store(true) }

// shouldRebuildLocalTree 判定本次全量扫描是否要从磁盘重建本地树（§5.3）。
// 中继/源端由 source 侧 watcher 维护树，不在此重建；纯汇端满足「距上次重建够久（标准节流）」
// 或「driftSuspected 置位（补充触发，无视节流）」才重建。driftSuspected 以 Swap 读并清零，
// 故本函数有副作用，每次全量扫描只调一次
func shouldRebuildLocalTree() bool {
	if config.ServesDownstream() {
		return false
	}
	return driftSuspected.Swap(false) || time.Since(lastLocalRebuild) >= localRebuildMinInterval
}

func runMirrorTasks(fileClient *network.FileClient) error {
	// 连接后先全量对账；重连（含休眠后 socket 断开）都会重新走到这里
	if err := executeTaskWithClient("initial full scan", fileClient, fullScan); err != nil {
		return err
	}

	// 有了实时推送，全量扫描退化为低频安全网
	fullScanInterval := time.Duration(*config.CoolDown) * time.Second
	lastFullScan := time.Now()

	for {
		// 长轮询：阻塞等待服务端推送变更（无变更时约 LongPollHold 后返回空）。
		// 空闲时客户端就阻塞在这一个 socket 读上，零轮询、零额外唤醒
		beforePoll := time.Now()
		if err := executeTaskWithClient("change tracking", fileClient, TrackingChanges); err != nil {
			return err
		}

		// 休眠感知：长轮询最多挂 ~60s，墙钟却跳了远超此值 → 刚从休眠醒来。
		// 服务端 changed_dirs 只保留 1 小时，睡久了增量窗口不可信，强制全量对账
		if elapsed := time.Since(beforePoll); elapsed > sleepDetectThreshold {
			log.Warnf("long sleep detected (%v), forcing a full reconciliation", elapsed.Round(time.Second))
			// 休眠期间本地可能被外部改动，强制从磁盘重建本地树、无视节流（§5.3 补充触发）
			SuspectLocalDrift()
			if err := executeTaskWithClient("post-wake full scan", fileClient, fullScan); err != nil {
				return err
			}
			lastFullScan = time.Now()
			continue
		}

		// 低频全量扫描安全网，兜住推送链路任何潜在遗漏
		if time.Since(lastFullScan) >= fullScanInterval {
			if err := executeTaskWithClient("full scan", fileClient, fullScan); err != nil {
				return err
			}
			lastFullScan = time.Now()
		}
	}
}

func fullScan(fileClient *network.FileClient) error {
	startTime := time.Now()

	// COR-01：纯汇端没有 fsnotify watcher，运行期本地漂移（备份目录被外部改/删/增）不会
	// 进树；而差异比对读的是 bbolt 缓存树、不是磁盘现状，漂移到重启前都不会被发现或修复。
	// 全量扫描是低频安全网，正好在此按磁盘现状重建一次本地树（BuildFileTree 的校准模式：
	// size+mtime 未变复用哈希、变了重算、磁盘已不存在的节点剔除），再与上游树比对，
	// 本地漂移即被纠正。仅限纯汇端：中继/源端由 source 侧 watcher 维护树，且并发重建会与
	// 之竞争同一棵树，故用 !ServesDownstream() 圈定
	// §5.3 节流：不是每次全量扫描都重建（判定见 shouldRebuildLocalTree）——距上次重建够久
	// 才做，或 driftSuspected 置位时强制做
	if shouldRebuildLocalTree() {
		if err := tree.BuildFileTree(config.StartPath); err != nil {
			log.Warnf("full scan: local disk re-calibration failed, proceeding with cached tree: %v", err)
		} else {
			lastLocalRebuild = time.Now()
		}
	}

	NextLevel.Clear()
	NextLevel.Push(DiffResult{
		Path:   ".",
		IsDir:  true,
		Action: "create",
		Name:   "root",
	})

	if err := drainNextLevel(fileClient, true); err != nil {
		return err
	}

	// 不用客户端时钟设置游标（会因时钟偏差漏查）。全量扫描后把游标重置为 0，
	// 下一次变更追踪以 [0, 服务端now] 全查一次窗口（此时多为已同步的空 diff），
	// 并从服务端返回的 CoveredUntil 重新确立游标，之后全程服务端时钟。
	// 这也顺带覆盖了扫描期间发生的变更，不会遗漏。
	lastChangeCursor = 0

	log.Infof("Full scan completed, total time taken: %v", time.Since(startTime))
	return nil
}

func TrackingChanges(fileClient *network.FileClient) error {
	change, coveredUntil, fullResync, err := fileClient.GetTreeChange(lastChangeCursor)
	if err != nil {
		return handleConnectionError(err, fileClient)
	}

	if fullResync {
		// 服务端本区间变更数超阈值，列表被省略：全量对账一次。
		// 注意 fullScan 会把游标归 0——若沿用，下一轮又会查到同一批超限
		// 变更再触发全量，活锁到日志窗口滑过为止。这里覆盖为本次响应的
		// CoveredUntil：全量扫描发生在响应之后，该时刻前的状态已被扫描覆盖
		log.Warnf("server reports too many changed directories in the window, falling back to a full reconciliation")
		if err := fullScan(fileClient); err != nil {
			return err
		}
		lastChangeCursor = coveredUntil
		return nil
	}

	if len(change) == 0 {
		// 长轮询保活返回，无变更；推进游标到服务端已覆盖时刻
		lastChangeCursor = coveredUntil
		return nil
	}
	allPaths := extractMinimalPathsFromChanges(change)
	NextLevel.Clear()
	// 本次变更批次内共享的失败隔离状态；不跨多次 TrackingChanges 调用持续，
	// 一个文件持续失败时下次心跳周期会重新尝试（成本很低，且能自愈）
	itemFailures := make(map[string]int)
	blacklist := make(map[string]bool)
	for _, v := range allPaths {
		log.Infof("Processing change: %v", v)
		err := getDirectory(fileClient, v, false, itemFailures, blacklist)
		if err == nil {
			continue
		}
		if errors.Is(err, errDirGone) {
			log.Debugf("source no longer has %s, skipping", v)
			continue
		}
		log.Errorf("Error processing directory %s: %v", v, err)
		if errors.Is(err, appError.ErrConnection) {
			if reconnectErr := fileClient.Reconnect(); reconnectErr != nil {
				return err
			}
		} else {
			// §5.1：非连接错误跳过该目录但计入统计，不静默当成功
			status.RecordError()
		}
	}
	// 变更中新出现的子目录需要继续下钻，否则要等下次全量扫描才能同步到
	if err := drainNextLevel(fileClient, false); err != nil {
		return err
	}
	// 游标推进到服务端本次已覆盖的时刻，不重叠不遗漏
	lastChangeCursor = coveredUntil
	return nil
}

func extractMinimalPathsFromChanges(changePaths []string) []string {
	var neededPaths []string
	processedPaths := make(map[string]bool)

	for _, path := range changePaths {
		if path == "" || path == "/" {
			continue
		}

		// 检查路径的父目录链，只添加不存在的父目录
		pathsToAdd := []string{path} // 总是包含变更的路径本身

		currentPath := filepath.Dir(path)
		for currentPath != "." && currentPath != "/" && currentPath != "" {
			// 检查父目录是否存在于本地
			exists, err := tree.HasPath(currentPath)
			if err != nil {
				log.Errorf("Error checking path %s: %v", currentPath, err)
				break
			}

			if !exists {
				pathsToAdd = append([]string{currentPath}, pathsToAdd...) // 前置插入
				currentPath = filepath.Dir(currentPath)
			} else {
				// 父目录存在，无需继续向上查找
				break
			}
		}

		// 添加到需要处理的路径列表
		for _, p := range pathsToAdd {
			if !processedPaths[p] {
				neededPaths = append(neededPaths, p)
				processedPaths[p] = true
			}
		}
	}

	// 按深度排序
	sort.Slice(neededPaths, func(i, j int) bool {
		depthI := strings.Count(neededPaths[i], string(filepath.Separator))
		depthJ := strings.Count(neededPaths[j], string(filepath.Separator))
		if depthI == depthJ {
			return neededPaths[i] < neededPaths[j]
		}
		return depthI < depthJ
	})

	return neededPaths
}
