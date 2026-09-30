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
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// initialPush 首次全量推送（汇端）：首次同步尚未完成、且源端支持时，由源端按固定顺序
// 一次推完整棵树，省掉逐目录、逐文件的请求往返。目标目录里原有的文件不影响推送（同名的
// 被覆盖）。推送结束后首次同步记录置 finalizing，随后的全量扫描负责收尾——补齐推送期间
// 的变更，并按 --allow-delete 处理源端没有的文件——成功后置 done（见 markInitialSyncDone）。
// 源端不支持、空间不足或源端中止推送时返回 nil，由全量扫描逐文件完成首次同步
func initialPush(fileClient *network.FileClient) error {
	rec, err := tree.LoadInitialSync()
	if err != nil {
		return err
	}
	if rec.State != tree.InitialSyncPending && rec.State != tree.InitialSyncRunning {
		return nil
	}
	if !fileClient.SupportsBulkPush() {
		log.Info("the source does not support the initial push; the first sync runs file by file")
		return nil
	}

	req := network.BulkRequestMessage{Ignore: config.IgnoreFileList, Cursor: rec.Cursor}
	plan, err := fileClient.BulkPlan(req)
	if err != nil {
		if errors.Is(err, appError.ErrConnection) {
			return handleConnectionError(err, fileClient)
		}
		log.Warnf("initial push unavailable (%v); the first sync runs file by file", err)
		return nil
	}
	if free, ferr := utils.DiskFree(config.StartPath); ferr == nil && free < plan.Bytes+diskReserve {
		log.Warnf("initial push needs %s but only %s is free (reserve %s); the first sync runs file by file",
			humanBytes(plan.Bytes), humanBytes(free), humanBytes(diskReserve))
		return nil
	}
	req.ResumePath, req.ResumeOffset = network.FindBulkResume()

	if rec.State == tree.InitialSyncPending {
		rec = tree.InitialSync{State: tree.InitialSyncRunning}
	}
	rec.TotalFiles, rec.TotalBytes = rec.Files+plan.Files, rec.Bytes+plan.Bytes
	if err := tree.SaveInitialSync(rec); err != nil {
		return err
	}
	if rec.Cursor == "" {
		log.Infof("initial push starting: %d files, %s", plan.Files, humanBytes(plan.Bytes))
	} else {
		log.Infof("initial push resuming after %s: %d files, %s left", rec.Cursor, plan.Files, humanBytes(plan.Bytes))
	}
	prevDetail := status.SetDetail(pushProgress(rec))
	defer status.SetDetail(prevDetail)

	var batch tree.Batch
	type dirTime struct {
		full string
		t    time.Time
	}
	var dirs []dirTime
	advance := func(path string, size uint64, file bool) {
		rec.Cursor = path
		if file {
			rec.Files++
			rec.Bytes += size
		}
		batch.SetInitialSync(rec)
		if file && rec.Files%100 == 0 {
			status.SetDetail(pushProgress(rec))
		}
	}
	handler := network.BulkHandler{
		Dir: func(e network.BulkEntryMessage) {
			full, err := safety.SafeResolve(config.StartPath, e.Path)
			if err != nil {
				log.Errorf("initial push: refusing out-of-root directory %s: %v", e.Path, err)
				return
			}
			if err := os.MkdirAll(full, 0o755); err != nil {
				log.Errorf("initial push: failed to create directory %s: %v", e.Path, err)
				return
			}
			v := DiffResult{Path: e.Path, IsDir: true, Mode: e.Mode, ModTime: time.Unix(0, e.ModTime)}
			applyPerm(full, v)
			dirs = append(dirs, dirTime{full, v.ModTime})
			batch.Add(pushedNode(e, full, ""), false)
			advance(e.Path, 0, false)
		},
		File: func(e network.BulkEntryMessage, hash string) {
			full := filepath.Join(config.StartPath, e.Path)
			applyModTime(DiffResult{Path: e.Path, ModTime: time.Unix(0, e.ModTime)})
			batch.Add(pushedNode(e, full, hash), true)
			advance(e.Path, e.Size, true)
			status.RecordFile(e.Path, e.Size)
		},
		FileFailed: func(e network.BulkEntryMessage, err error) {
			log.Warnf("initial push: %s not received (%v); the full scan after the push retries it", e.Path, err)
			advance(e.Path, 0, false)
		},
	}
	end, err := fileClient.BulkPush(req, handler)
	batch.Commit()
	if err != nil {
		if errors.Is(err, appError.ErrConnection) {
			log.Warnf("initial push interrupted after %d/%d files; it resumes on the next connection", rec.Files, rec.TotalFiles)
			return handleConnectionError(err, fileClient)
		}
		log.Warnf("initial push aborted by the source (%v); the first sync continues file by file", err)
		return nil
	}

	// 目录的修改时间在其内容写完后统一回填，子项先于父目录
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chtimes(dirs[i].full, dirs[i].t, dirs[i].t); err != nil {
			log.Debugf("failed to set mtime for %s: %v", dirs[i].full, err)
		}
	}
	rec.State, rec.Cursor = tree.InitialSyncFinalizing, ""
	if err := tree.SaveInitialSync(rec); err != nil {
		return err
	}
	if config.ServesDownstream() {
		tree.AddRecentChangedDir(".")
	}
	// 收尾的全量扫描先按磁盘校准本地树：目标目录里原有、源端没有的文件由此进树，再交给 diff
	SuspectLocalDrift()
	log.Infof("initial push done: %d files, %s received in this session", end.Files, humanBytes(end.Bytes))
	return nil
}

// markInitialSyncDone 首次同步后的第一次全量扫描成功即完成首次同步：推送收尾完毕，
// 或源端不支持推送、已逐文件同步完
func markInitialSyncDone() {
	rec, err := tree.LoadInitialSync()
	if err != nil || rec.State == tree.InitialSyncDone {
		return
	}
	if err := tree.SaveInitialSync(tree.InitialSync{State: tree.InitialSyncDone, Files: rec.Files, Bytes: rec.Bytes}); err != nil {
		log.Errorf("recording the completed first sync failed: %v", err)
	}
}

// pushedNode 由推送条目与落盘后的实际文件构造树节点。ParentID 留空：父目录可能与它同批
// 尚未提交，入库时按父路径解析
func pushedNode(e network.BulkEntryMessage, full, hash string) *tree.Node {
	id, _ := utils.RandomString(16)
	modTime := time.Unix(0, e.ModTime)
	if fi, err := os.Stat(full); err == nil {
		modTime = fi.ModTime()
	}
	return &tree.Node{
		ID:      id,
		Path:    e.Path,
		Name:    filepath.Base(e.Path),
		IsDir:   e.Kind == network.BulkEntryDir,
		Size:    e.Size,
		ModTime: modTime,
		Hash:    hash,
		Depth:   strings.Count(e.Path, string(filepath.Separator)),
		Mode:    e.Mode,
	}
}

func pushProgress(rec tree.InitialSync) string {
	return fmt.Sprintf("initial push: %d/%d files, %s/%s",
		rec.Files, rec.TotalFiles, humanBytes(rec.Bytes), humanBytes(rec.TotalBytes))
}
