package network

import (
	"errors"
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/internal/safety"
	"net"
	"os"
	"path/filepath"
)

// BulkPlan 询问源端本次全量推送的文件数与字节数
func (c *FileClient) BulkPlan(req BulkRequestMessage) (BulkPlanResponseMessage, error) {
	var plan BulkPlanResponseMessage
	conn, err := c.connectionManage.GetConnection()
	if err != nil {
		return plan, fmt.Errorf("%w: failed to get connection: %v", appError.ErrConnection, err)
	}
	if err := sendMessage(conn, MsgTypeBulkPlanRequest, encodeBulkRequest(req)); err != nil {
		return plan, fmt.Errorf("%w: failed to send bulk plan request: %v", appError.ErrConnection, err)
	}
	msgType, body, err := receiveMessage(conn)
	if err != nil {
		return plan, fmt.Errorf("%w: failed to receive bulk plan: %v", appError.ErrConnection, err)
	}
	switch msgType {
	case MsgTypeBulkPlanResponse:
		if plan, err = decodeBulkPlanResponse(body); err != nil {
			return plan, fmt.Errorf("%w: failed to decode bulk plan: %v", appError.ErrConnection, err)
		}
		return plan, nil
	case MsgTypeError:
		return plan, realityErrorFrom(body)
	default:
		return plan, fmt.Errorf("%w: invalid bulk plan response type %d", appError.ErrConnection, msgType)
	}
}

// BulkHandler 汇端对推送流中各条目的处理。File 在文件已校验并落到最终位置后调用；
// FileFailed 在单个文件接收失败（非连接错误）时调用，推送继续
type BulkHandler struct {
	Dir        func(e BulkEntryMessage)
	File       func(e BulkEntryMessage, hash string)
	FileFailed func(e BulkEntryMessage, err error)
}

// BulkPush 请求源端开始全量推送并逐条处理，直到收到结束标记。连接错误时返回
// appError.ErrConnection，已处理的条目不受影响，调用方据续推游标重新发起
func (c *FileClient) BulkPush(req BulkRequestMessage, h BulkHandler) (BulkEndMessage, error) {
	var end BulkEndMessage
	conn, err := c.connectionManage.GetConnection()
	if err != nil {
		return end, fmt.Errorf("%w: failed to get connection: %v", appError.ErrConnection, err)
	}
	if err := sendMessage(conn, MsgTypeBulkStartRequest, encodeBulkRequest(req)); err != nil {
		return end, fmt.Errorf("%w: failed to send bulk start request: %v", appError.ErrConnection, err)
	}
	for {
		msgType, body, err := receiveMessage(conn)
		if err != nil {
			return end, fmt.Errorf("%w: failed to receive bulk push: %v", appError.ErrConnection, err)
		}
		switch msgType {
		case MsgTypeBulkEntry:
			e, err := decodeBulkEntry(body)
			if err != nil {
				return end, fmt.Errorf("%w: failed to decode bulk entry: %v", appError.ErrConnection, err)
			}
			switch e.Kind {
			case BulkEntryDir:
				h.Dir(e)
			case BulkEntryFile:
				hash, err := receiveBulkFile(conn, e)
				if err != nil {
					if errors.Is(err, appError.ErrConnection) {
						return end, err
					}
					h.FileFailed(e, err)
					continue
				}
				h.File(e, hash)
			default:
				return end, fmt.Errorf("%w: unknown bulk entry kind %d", appError.ErrConnection, e.Kind)
			}
		case MsgTypeBulkEnd:
			if end, err = decodeBulkEnd(body); err != nil {
				return end, fmt.Errorf("%w: failed to decode bulk end: %v", appError.ErrConnection, err)
			}
			return end, nil
		case MsgTypeError:
			return end, realityErrorFrom(body)
		default:
			return end, fmt.Errorf("%w: unexpected message type %d during bulk push", appError.ErrConnection, msgType)
		}
	}
}

// receiveBulkFile 接收推送流中的一个文件。源端从 e.Offset 续传时，本地分片必须正好
// 接得上（同一文件指纹、同样长度），否则排空该会话并报错，由推送后的全量比对补回
func receiveBulkFile(conn net.Conn, e BulkEntryMessage) (string, error) {
	if _, err := safety.SafeJoin(config.StartPath, e.Path); err != nil {
		return "", drainAfter(conn, fmt.Errorf("refusing to write out-of-root path: %w", err))
	}
	partialPath, metaPath := partialPaths(e.Path)
	if err := os.MkdirAll(filepath.Dir(partialPath), 0o755); err != nil {
		return "", drainAfter(conn, fmt.Errorf("failed to create partial dir: %w", err))
	}
	fs := fileSession{Path: e.Path, Perm: e.Mode, ID: e.SessionID, Size: e.Size,
		StartHash: fmt.Sprintf("%x", e.FileHash), Offset: e.Offset, Bulk: true}
	if e.Offset > 0 {
		have, meta := loadPartialState(partialPath, metaPath)
		if meta == nil || have != e.Offset || meta.Hash != fs.StartHash || meta.Size != e.Size {
			discardPartial(partialPath, metaPath)
			return "", drainAfter(conn, fmt.Errorf("partial data for %s does not match the resumed stream", e.Path))
		}
		fs.Resume = true
	}
	return receiveFile(conn, fs)
}

// FindBulkResume 查找上次推送中断时留下的分片，返回其同步路径与已有长度；没有则返回空
func FindBulkResume() (string, uint64) {
	dir := filepath.Join(config.StartPath, ".local-mirror", "partial")
	metas, _ := filepath.Glob(filepath.Join(dir, "*.meta"))
	for _, metaPath := range metas {
		partialPath := metaPath[:len(metaPath)-len(".meta")] + ".part"
		have, meta := loadPartialState(partialPath, metaPath)
		if meta != nil && meta.Path != "" && have > 0 {
			return filepath.FromSlash(meta.Path), have
		}
	}
	return "", 0
}

// FetchFile 批量下载中的一个文件。Perm 同 DownloadFile 的 perm（上游权限位，0 = 未知）
type FetchFile struct {
	Path string
	Perm uint32
}

// FetchFiles 增量同步的批量下载（需对端支持，见 SupportsFetchBatch）：一个请求取回整组文件，
// 源端连续推送，N 个文件只付一个往返，而不是逐个 FileRequest 的 N 个。每个文件的接收与
// DownloadFile 完全相同（receiveRequested → receiveFile：整文件哈希校验、覆盖前快照、
// 落盘前的符号链接复验、分片续传与过期分片的处理）。
//
// done 对每个文件回调一次（本地就拒绝的在发请求前回调，其余按请求顺序）：err 为 nil 时
// 文件已落到最终位置、hash 为其十六进制哈希；否则为该文件的失败（源端给不出时是
// *RealityError）。返回值只报流已不可续读的连接错误（appError.ErrConnection），调用方须
// 关闭连接；能归到某个文件的连接错误已先随该文件回调过，其后的文件不再回调
func (c *FileClient) FetchFiles(files []FetchFile, done func(i int, hash string, err error)) error {
	// 请求里的每一项：在 files 中的下标，以及发请求时据本地分片定下的续传起点
	type pending struct {
		i      int
		offset uint64
		prev   *partialMeta
	}
	var req FetchRequestMessage
	var pend []pending
	for i, f := range files {
		// 同 DownloadFile：路径来自服务端下发的目录树，越界的连请求都不发
		if _, err := safety.SafeJoin(config.StartPath, f.Path); err != nil {
			done(i, "", fmt.Errorf("refusing to download out-of-root path: %w", err))
			continue
		}
		partialPath, metaPath := partialPaths(f.Path)
		if err := os.MkdirAll(filepath.Dir(partialPath), 0755); err != nil {
			done(i, "", fmt.Errorf("failed to create partial dir: %w", err))
			continue
		}
		offset, prev := loadPartialState(partialPath, metaPath)
		req.Items = append(req.Items, FetchItem{Path: f.Path, Offset: offset})
		pend = append(pend, pending{i: i, offset: offset, prev: prev})
	}
	if len(pend) == 0 {
		return nil
	}
	fail := func(k int, err error) error {
		done(pend[k].i, "", err)
		return err
	}

	conn, err := c.connectionManage.GetConnection()
	if err != nil {
		return fail(0, fmt.Errorf("%w: failed to get connection: %v", appError.ErrConnection, err))
	}
	if err := sendMessage(conn, MsgTypeFetchRequest, encodeFetchRequest(req)); err != nil {
		return fail(0, fmt.Errorf("%w: failed to send fetch request: %v", appError.ErrConnection, err))
	}
	for k, p := range pend {
		f := files[p.i]
		msgType, body, err := receiveMessage(conn)
		if err != nil {
			return fail(k, fmt.Errorf("%w: failed to receive message: %v", appError.ErrConnection, err))
		}
		switch msgType {
		case MsgTypeError:
			// 服务端已无法提供该文件（如已被删除），分片不再有保留价值
			discardPartial(partialPaths(f.Path))
			done(p.i, "", realityErrorFrom(body))
		case MsgTypeBulkEntry:
			e, err := decodeBulkEntry(body)
			if err != nil {
				return fail(k, fmt.Errorf("%w: failed to decode fetch entry: %v", appError.ErrConnection, err))
			}
			// 应答必须与请求逐项对齐：错位说明流已不可信，按连接错误断开重来
			if e.Kind != BulkEntryFile || e.Path != f.Path || e.Offset != p.offset {
				return fail(k, fmt.Errorf("%w: fetch entry %q (kind %d, offset %d) does not match request %q (offset %d)",
					appError.ErrConnection, e.Path, e.Kind, e.Offset, f.Path, p.offset))
			}
			hash, err := receiveRequested(conn, f.Path, f.Perm, p.offset, p.prev, e.SessionID, e.Size, e.FileHash)
			if err != nil && errors.Is(err, appError.ErrConnection) {
				return fail(k, err)
			}
			done(p.i, hash, err)
		default:
			return fail(k, fmt.Errorf("%w: unexpected message type %d during fetch", appError.ErrConnection, msgType))
		}
	}
	msgType, body, err := receiveMessage(conn)
	if err != nil {
		return fmt.Errorf("%w: failed to receive fetch end: %v", appError.ErrConnection, err)
	}
	if msgType != MsgTypeBulkEnd {
		return fmt.Errorf("%w: expected fetch end, got message type %d", appError.ErrConnection, msgType)
	}
	if _, err := decodeBulkEnd(body); err != nil {
		return fmt.Errorf("%w: failed to decode fetch end: %v", appError.ErrConnection, err)
	}
	return nil
}
