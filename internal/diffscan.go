package app

import (
	"errors"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/internal/network"
	"local-mirror/internal/safety"
	"local-mirror/internal/status"
	"local-mirror/internal/tree"
	"local-mirror/pkg/utils"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
)

// maxItemRetries 单个 diff 项（通常是文件）连续触发连接错误后拉黑的次数上限。
// 目录内某一项持续失败（权限、磁盘满等本地错误）不应该无限期拖累同目录
// 其余正常文件的同步——拉黑后该项在本轮内不再尝试，其余项照常处理。
//
// 不变量：必须满足 maxItemRetries <= maxDirRetries。问题文件的前
// maxItemRetries 次失败会累积目录级失败计数（拉黑发生后才清零），
// 若本值更大，目录会在文件被拉黑之前先耗尽重试预算被整体放弃。
const maxItemRetries = 3

// errDirGone 源端树里已经没有这个目录了。变更日志推来的路径可能在客户端
// 来访之前就被删掉（创建后又删除是开发中的常态），全量扫描逐层下钻时也会
// 撞上同样的竞态。这不是故障：本地副本的清理由父目录的 diff 负责，
// 静默跳过即可，按 error 记只会把常规操作刷成满屏告警
var errDirGone = errors.New("directory no longer exists on the source")

// dirGone 判定服务端的目录列表应答是不是"该目录已不存在"，是则转成 errDirGone，
// 否则返回 nil 表示不属此类、交给调用方按其他错误处理
func dirGone(err error, path string) error {
	var re *network.RealityError
	if errors.As(err, &re) && re.Code == network.ErrCodeNotFound {
		return fmt.Errorf("%w: %s", errDirGone, path)
	}
	return nil
}

// getDirectory 同步单个目录：拉取服务端目录列表、执行差异处理，
// 并把需要继续下钻的子目录压入 NextLevel。
// recurseAll 为 true 时所有子目录都下钻（全量扫描的安全网语义）；
// 为 false 时只下钻本次新建/变更的子目录。
// itemFailures/blacklist 是跨多次目录重试共享的状态（调用方持有），用于把
// 持续失败的具体路径隔离掉，使目录内其余正常项不受拖累。
func getDirectory(fileClient *network.FileClient, path string, recurseAll bool, itemFailures map[string]int, blacklist map[string]bool) error {
	// 客户端忽略：命中忽略列表的目录整体跳过（变更追踪可能推来
	// 忽略目录内的深层路径，连目录列表请求都不必发）
	if utils.IsIgnored(path, config.IgnoreFileList) {
		log.Debugf("skipping ignored directory: %s", path)
		return nil
	}
	// 树响应按页下发并在客户端内聚合（超大目录不再撞消息体上限），
	// 返回的节点路径已是本机分隔符格式
	realityNodes, err := fileClient.GetRealityTree(path)
	if err != nil {
		if gone := dirGone(err, path); gone != nil {
			return gone
		}
		return handleConnectionError(err, fileClient)
	}

	diffs, err := Diff(realityNodes, path)
	if err != nil {
		return fmt.Errorf("error analyzing diff for path %s: %w", path, err)
	}

	// 客户端忽略：命中项从 diff 中整体剔除——create/modify 不下载、
	// delete 不删除、也不参与后面的重命名配对。服务端未忽略而客户端
	// 忽略的条目由此对同步完全隐形（本地已有的副本也不会被碰）
	diffs = filterIgnoredDiffs(diffs)

	// 保真：就地重命名的文件走本地 rename，免整文件重新下载（COR-02，门控见 maybeDetectRenames）
	diffs = maybeDetectRenames(diffs)

	log.Infof("Diff count for %s: %d", path, len(diffs))
	diffDirs := make(map[string]bool)
	diskFullSkipped := 0
	for _, v := range diffs {
		if blacklist[v.Path] {
			// 已确认持续失败，本轮不再尝试，让其余正常项能被处理到
			continue
		}
		if err := processDiffItem(v, fileClient); err != nil {
			// 磁盘空间不足：跳过该文件继续处理其余项（小文件可能仍装得下），
			// 目录处理完后聚合成一条提示，避免逐文件刷屏
			if errors.Is(err, appError.ErrDiskFull) {
				diskFullSkipped++
				log.Debugf("skipped for low disk space: %v", err)
				continue
			}
			// 连接断了：无论是否拉黑，这次调用都不能继续复用这个连接处理
			// 剩余项，必须整体返回交给上层重连后重试；其他错误跳过单项继续
			if errors.Is(err, appError.ErrConnection) {
				itemFailures[v.Path]++
				if itemFailures[v.Path] > maxItemRetries {
					blacklist[v.Path] = true
					log.Errorf("%s failed %d times in a row, giving it up for this round (other files unaffected)", v.Path, itemFailures[v.Path]-1)
				}
				return err
			}
			log.Errorf("Error processing diff item %v: %v", v, err)
			continue
		}
		recordChangedDir(v.Path)
		if v.IsDir && v.Action != "delete" {
			diffDirs[v.Path] = true
			NextLevel.Push(v)
		}
	}
	if diskFullSkipped > 0 {
		free, _ := utils.DiskFree(config.StartPath)
		log.Errorf("directory %s: %d files skipped for low disk space (%s free, %s reserved); they will catch up automatically once space is freed",
			path, diskFullSkipped, humanBytes(free), humanBytes(diskReserve))
	}

	if recurseAll {
		for _, node := range realityNodes {
			if node.IsDir && utils.IsIgnored(node.Path, config.IgnoreFileList) {
				// 忽略目录不下钻（服务端可能没忽略它，树里存在）
				continue
			}
			if node.IsDir && !diffDirs[node.Path] {
				NextLevel.Push(DiffResult{
					Path:   node.Path,
					IsDir:  true,
					Action: "modify",
					Name:   node.Name,
					Size:   node.Size,
				})
			}
		}
	}
	return nil
}

// filterIgnoredDiffs 剔除命中忽略列表的 diff 项。
// 客户端忽略语义：不下载（create/modify）、不删除（delete）——
// 即便服务端树里有该条目，也当它不存在；本地磁盘上的同名内容原样保留
func filterIgnoredDiffs(diffs []DiffResult) []DiffResult {
	kept := diffs[:0]
	for _, d := range diffs {
		if utils.IsIgnored(d.Path, config.IgnoreFileList) {
			log.Debugf("ignoring diff item (%s): %s", d.Action, d.Path)
			continue
		}
		kept = append(kept, d)
	}
	return kept
}

// maybeDetectRenames 仅在 --allow-delete（忠实镜像）下启用重命名优化——rename 会移除
// 旧路径，本质是删除；默认增量模式不删除，那里旧文件必须原样保留、新文件另行下载，
// 否则「默认只同步不删」会被这条优化悄悄打破（COR-02）
func maybeDetectRenames(diffs []DiffResult) []DiffResult {
	if !*config.AllowDelete {
		return diffs
	}
	return detectRenames(diffs)
}

// detectRenames 在单个目录的 diff 内识别"就地重命名"：一个 delete 与一个
// create 若指向哈希相同的文件（内容未变、仅换名），直接本地 rename，
// 避免整文件重新下载。返回消化掉重命名对之后剩余的 diff。
// 仅处理同目录内的文件（跨目录移动分属不同目录的 diff，无法在此配对）。
func detectRenames(diffs []DiffResult) []DiffResult {
	// 按哈希索引待删除的文件（每个哈希取第一个）
	delIdxByHash := make(map[string]int)
	for i, d := range diffs {
		if d.Action == "delete" && !d.IsDir && d.Hash != "" {
			if _, exists := delIdxByHash[d.Hash]; !exists {
				delIdxByHash[d.Hash] = i
			}
		}
	}
	if len(delIdxByHash) == 0 {
		return diffs
	}

	handled := make(map[int]bool)
	for i, d := range diffs {
		if d.Action != "create" || d.IsDir || d.Hash == "" {
			continue
		}
		di, ok := delIdxByHash[d.Hash]
		if !ok || handled[di] || diffs[di].Path == d.Path {
			continue
		}
		if err := applyRename(diffs[di], d); err != nil {
			log.Warnf("rename %s -> %s failed, falling back to download: %v", diffs[di].Path, d.Path, err)
			continue
		}
		handled[i] = true
		handled[di] = true
		log.Infof("move detected: %s -> %s (local rename, no download)", diffs[di].Path, d.Path)
	}
	if len(handled) == 0 {
		return diffs
	}

	remaining := make([]DiffResult, 0, len(diffs)-len(handled))
	for i, d := range diffs {
		if !handled[i] {
			remaining = append(remaining, d)
		}
	}
	return remaining
}

// applyRename 执行一次就地重命名：本地移动文件、对齐 mtime、更新数据库
func applyRename(oldDiff, newDiff DiffResult) error {
	oldFull, err := safety.SafeResolve(config.StartPath, oldDiff.Path)
	if err != nil {
		log.Errorf("refusing to rename from out-of-root path: %v", err)
		return nil
	}
	newFull, err := safety.SafeResolve(config.StartPath, newDiff.Path)
	if err != nil {
		log.Errorf("refusing to rename to out-of-root path: %v", err)
		return nil
	}
	// COR-02：rename 前重验本地旧文件，不信任数据库里的旧哈希。若本地已漂移（内容被
	// 外部改动而 DB 未更新、被换成目录/软链、或已消失），把「错内容」搬到新路径并登记成
	// 上游哈希会造成静默损坏。校验不过就返回错误——detectRenames 会据此放弃这对配对，
	// 回落到正常的 delete+download，取到的是上游的正确内容
	linfo, lerr := os.Lstat(oldFull)
	if lerr != nil || !linfo.Mode().IsRegular() {
		return fmt.Errorf("local source not a regular file (drifted or gone): %s", oldDiff.Path)
	}
	if h, herr := utils.CalcBlake3(oldFull); herr != nil || fmt.Sprintf("%x", h) != oldDiff.Hash {
		return fmt.Errorf("local source hash mismatch (drifted): %s", oldDiff.Path)
	}
	if err := os.MkdirAll(filepath.Dir(newFull), 0755); err != nil {
		return err
	}
	if err := os.Rename(oldFull, newFull); err != nil {
		return err
	}
	applyModTime(newDiff)
	if err := tree.DeleteNode(oldDiff.Path); err != nil {
		return err
	}
	if err := tree.AddNodes([]*tree.Node{createNodeFromDiff(newDiff, newDiff.Hash)}); err != nil {
		return err
	}
	// 重命名影响新旧两个父目录
	recordChangedDir(oldDiff.Path)
	recordChangedDir(newDiff.Path)
	return nil
}

// maxDirRetries 单个目录连续失败后放弃的次数上限。
// 若失败原因是持续性的本地错误（权限、磁盘满等），每次重连后立即重试会
// 无限速循环——之前一次 ulimit 复现里，1 秒内触发了 1300+ 次重连。
// 必须既限制重试次数、又在重试前退避，而不是任由其中一种机制单独兜底
const maxDirRetries = 3

// drainNextLevel 逐层消费 NextLevel 中的目录，连接错误时重连并重试当前目录。
// 同一目录连续失败达到上限后放弃该目录（记录错误），避免持续性本地错误
// 导致无退避的重连风暴
func drainNextLevel(fileClient *network.FileClient, recurseAll bool) error {
	retries := make(map[string]int)
	// itemFailures/blacklist 跨目录的多次重试持续存在：目录内某个具体文件
	// 反复失败会被拉黑（见 getDirectory），使同目录其余正常文件不被拖累
	itemFailures := make(map[string]int)
	blacklist := make(map[string]bool)

	for NextLevel.Size() > 0 {
		v, _ := NextLevel.Pop()
		log.Debugf("Processing next level item: %v 【%d】remaining", v, NextLevel.Size())

		if !v.IsDir {
			log.Error("Unexpected file type in NextLevel stack, expected directory but got file:", v.Path)
			continue
		}

		blacklistBefore := len(blacklist)
		err := getDirectory(fileClient, v.Path, recurseAll, itemFailures, blacklist)
		if err == nil {
			continue
		}
		if errors.Is(err, errDirGone) {
			log.Debugf("source no longer has %s, skipping", v.Path)
			continue
		}

		log.Errorf("Error processing directory %s: %v", v.Path, err)
		if errors.Is(err, appError.ErrConnection) {
			if len(blacklist) > blacklistBefore {
				// 本轮拉黑了一个新的问题文件，说明在收敛（diff 下一轮会变小），
				// 不计入目录失败次数，避免"目录内有多个问题文件"时，
				// 目录级重试预算在文件逐个被拉黑之前就被耗尽
				retries[v.Path] = 0
			} else {
				retries[v.Path]++
			}
			if retries[v.Path] > maxDirRetries {
				log.Errorf("directory %s failed %d times in a row, giving up this round", v.Path, retries[v.Path]-1)
				continue
			}
			if reconnectErr := fileClient.Reconnect(); reconnectErr != nil {
				return err
			}
			// 退避后再重试：给持续性错误留出恢复窗口，也避免忙循环
			time.Sleep(time.Duration(retries[v.Path]) * time.Second)
			NextLevel.Push(v)
		} else {
			// §5.1：非连接错误（Diff/DB 等）跳过该目录、继续同步其余目录（一个坏目录不该
			// 拖垮整轮），但计入错误统计——别让「某目录本轮没同步成」静默地当成成功
			status.RecordError()
		}
	}
	return nil
}
