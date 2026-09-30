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
