package network

import (
	"bufio"
	"errors"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/internal/tree"
	"local-mirror/pkg/utils"
	"net"
	"path/filepath"
	"sort"
	"strings"

	log "github.com/sirupsen/logrus"
)

// 首次全量推送（源端）：汇端尚未完成首次同步时，按固定顺序把整棵树一次推完，
// 省掉逐目录列清单、逐文件请求的往返。放行规则与单文件请求相同（openServeFile）

// compareTreeOrder 按推送顺序比较两个相对路径：逐级比较路径组件（字节序），
// 祖先排在子孙之前。与 bulkWalk 的深度优先、同目录按名字排序一致
func compareTreeOrder(a, b string) int {
	as := strings.Split(a, string(filepath.Separator))
	bs := strings.Split(b, string(filepath.Separator))
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := strings.Compare(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return len(as) - len(bs)
}

func isAncestorPath(dir, p string) bool {
	return strings.HasPrefix(p, dir+string(filepath.Separator))
}

// bulkWalk 按推送顺序遍历源端树，对每个应推送的条目调用 visit：深度优先，同目录按名字
// 字节序，目录先于其内容。跳过状态目录、两端的忽略项与哈希缺失（读不了）的文件，
// 以及遍历顺序中不晚于续推游标的条目
func bulkWalk(dir string, req BulkRequestMessage, visit func(tree.Node) error) error {
	entries, err := tree.GetDirContents(dir)
	if err != nil {
		if errors.Is(err, tree.ErrDirNotFound) {
			return nil
		}
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	for _, n := range entries {
		if n.Path == localMirrorStateDir || utils.IsIgnored(n.Path, config.IgnoreFileList) || utils.IsIgnored(n.Path, req.Ignore) {
			continue
		}
		if !n.IsDir && n.Hash == "" {
			continue
		}
		passed := req.Cursor == "" || compareTreeOrder(n.Path, req.Cursor) > 0
		if passed {
			if err := visit(n); err != nil {
				return err
			}
		}
		// 游标所在的目录（或其祖先）本身已推过，但其后的内容仍要推
		if n.IsDir && (passed || n.Path == req.Cursor || isAncestorPath(n.Path, req.Cursor)) {
			if err := bulkWalk(n.Path, req, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *fileServer) handleBulkPlan(c *client, body []byte) error {
	if err := requireHandshake(c); err != nil {
		return err
	}
	req, err := decodeBulkRequest(body)
	if err != nil {
		return fmt.Errorf("%w, error decoding bulk plan request: %v", appError.ErrConnection, err)
	}
	var plan BulkPlanResponseMessage
	if err := bulkWalk(".", req, func(n tree.Node) error {
		if !n.IsDir {
			plan.Files++
			plan.Bytes += n.Size
		}
		return nil
	}); err != nil {
		return err
	}
	if err := sendMessage(c.Conn, MsgTypeBulkPlanResponse, encodeBulkPlanResponse(plan)); err != nil {
		return fmt.Errorf("%w, error sending bulk plan: %v", appError.ErrConnection, err)
	}
	return nil
}

// bufferedConn 推送期间攒满缓冲再写出：连续的小消息合并成少量系统调用与加密帧
type bufferedConn struct {
	net.Conn
	w *bufio.Writer
}

func (b *bufferedConn) Write(p []byte) (int, error) { return b.w.Write(p) }

func (s *fileServer) handleBulkStart(c *client, body []byte) error {
	if err := requireHandshake(c); err != nil {
		return err
	}
	req, err := decodeBulkRequest(body)
	if err != nil {
		return fmt.Errorf("%w, error decoding bulk start request: %v", appError.ErrConnection, err)
	}
	conn := &bufferedConn{Conn: c.Conn, w: bufio.NewWriterSize(c.Conn, 256<<10)}
	var end BulkEndMessage
	err = bulkWalk(".", req, func(n tree.Node) error {
		if n.IsDir {
			entry := BulkEntryMessage{Kind: BulkEntryDir, Path: n.Path, Mode: n.Mode, ModTime: n.ModTime.UnixNano()}
			if err := sendMessage(conn, MsgTypeBulkEntry, encodeBulkEntry(entry)); err != nil {
				return fmt.Errorf("%w, error sending bulk entry %s: %v", appError.ErrConnection, n.Path, err)
			}
			return nil
		}
		var offset uint64
		if n.Path == req.ResumePath && req.ResumeOffset <= n.Size {
			offset = req.ResumeOffset
		}
		sent, err := s.pushFile(conn, c, n, offset)
		if err != nil {
			var we *wireError
			if errors.As(err, &we) || !errors.Is(err, appError.ErrConnection) {
				// 推送时才发现给不出（已删除、读不了等）：跳过，由推送后的全量比对收尾
				log.Debugf("bulk push skipping %s: %v", n.Path, err)
				return nil
			}
			return err
		}
		end.Files++
		end.Bytes += sent
		return nil
	})
	if err != nil {
		return err
	}
	if err := sendMessage(conn, MsgTypeBulkEnd, encodeBulkEnd(end)); err != nil {
		return fmt.Errorf("%w, error sending bulk end: %v", appError.ErrConnection, err)
	}
	if err := conn.w.Flush(); err != nil {
		return fmt.Errorf("%w, error flushing bulk push: %v", appError.ErrConnection, err)
	}
	log.Infof("Bulk push to %s done: %d files, %d bytes", c.Addr, end.Files, end.Bytes)
	return nil
}

// pushFile 推送一个文件条目及其数据，返回本次实际发送的字节数（续传时不含已有部分）
func (s *fileServer) pushFile(conn net.Conn, c *client, n tree.Node, offset uint64) (uint64, error) {
	sf, release, err := openServeFile(n.Path, offset)
	if err != nil {
		return 0, err
	}
	defer release()
	defer sf.file.Close()

	sessionID, err := utils.RandomString(16)
	if err != nil {
		return 0, fmt.Errorf("error generating session ID for file %s", n.Path)
	}
	var id [16]byte
	copy(id[:], sessionID)
	size := uint64(sf.info.Size())
	entry := BulkEntryMessage{
		Kind:      BulkEntryFile,
		Path:      n.Path,
		Mode:      n.Mode,
		ModTime:   n.ModTime.UnixNano(),
		Size:      size,
		Offset:    sf.offset,
		SessionID: id,
		FileHash:  sf.hash,
	}
	if err := sendMessage(conn, MsgTypeBulkEntry, encodeBulkEntry(entry)); err != nil {
		return 0, fmt.Errorf("%w, error sending bulk entry %s: %v", appError.ErrConnection, n.Path, err)
	}
	sess := &session{ID: id, FilePath: sf.fullPath, FileSize: size, Offset: sf.offset, file: sf.file}
	c.SessionMap.Store(sess.ID, sess)
	if err := s.sendFileData(conn, c, sess); err != nil {
		// 条目已发出，数据流中途失败无法在流内收尾，只能断开由汇端续推
		if !errors.Is(err, appError.ErrConnection) {
			err = fmt.Errorf("%w, %v", appError.ErrConnection, err)
		}
		return 0, err
	}
	return size - sf.offset, nil
}
