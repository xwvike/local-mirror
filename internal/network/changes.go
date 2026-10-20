package network

import (
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/appError"
	"local-mirror/internal/tree"
	"local-mirror/pkg/utils"
	"time"

	log "github.com/sirupsen/logrus"
)

// changeFullResyncThreshold 变更响应降级阈值。单次区间查询命中的变更目录
// 超过此数时不再下发列表，改为 FullResync 信号让客户端全量对账——
// 既避免响应逼近消息体上限（此前是确定性失败 + 最长 1 小时的重连活锁），
// 也因为处理上万条目录 diff 本就比一次全量扫描更慢
const changeFullResyncThreshold = 8192

// buildRecentChangeResponse 组装变更响应：数量超过阈值时降级为 FullResync
// 信号（列表省略）。纯函数，便于单元测试
func buildRecentChangeResponse(changes []string, covered int64) RecentChangeResponseMessage {
	unique := utils.UniqueStrings(changes)
	if len(unique) > changeFullResyncThreshold {
		return RecentChangeResponseMessage{
			ServerID:     config.InstanceID,
			CoveredUntil: covered,
			FullResync:   true,
		}
	}
	return RecentChangeResponseMessage{
		ServerID:     config.InstanceID,
		CoveredUntil: covered,
		Changes:      unique,
	}
}

func (s *fileServer) handleRecentChangeRequest(c *client, bodyBytes []byte) error {
	// 与其余 handler 一致：未握手的连接按连接错误关闭，
	// 而不是静默不应答让对端干等读超时
	if err := requireHandshake(c); err != nil {
		return err
	}
	conn := c.Conn
	recentChangeRequest, err := decodeRecentChangeRequest(bodyBytes)
	if err != nil {
		return fmt.Errorf("%w, error decoding recent change request: %v", appError.ErrConnection, err)
	}
	log.Debugf("Received recent change request from %s, client ID: %d, startTime: %d",
		conn.RemoteAddr().String(), recentChangeRequest.ClientID, recentChangeRequest.StartTime)

	// 长轮询：区间内已有变更立刻回（追赶/重连场景）；无变更则挂起，
	// 等到变更落库广播或挂满上限后返回。挂起期间不读 socket，
	// 上限兜底避免死连接常驻。上界用服务端当前时刻，随每次唤醒重新求值。
	//
	// 只交付已经"定型"的秒（键 < cutoff），覆盖点给 cutoff。客户端下次从覆盖点起查
	// （闭区间），于是每条记录恰好交付一次：
	//   - 原先覆盖点给 now、交付到 now（含）：同一秒里写入的记录会在这一秒内被
	//     反复交付——汇端处理完立刻再查，又拿到同一条，实测一次新建文件引发
	//     5000+ 轮空转（每轮一次整目录清单），直到时钟跨过这一秒；
	//   - 记录键在提交前就定了，提交（含 fsync）之后才可见，查询恰好落在两者之间
	//     又跨了秒的话，这条记录会被覆盖点跨过去、只能等全量扫描。
	// 定型的边界由 tree.ChangeLogSettled 给出：当前秒与在途写事务的较早者
	start := recentChangeRequest.StartTime
	holdDeadline := time.Now().Add(LongPollHold)
	for {
		// 先取信号再查询：若广播发生在查询之后、select 之前，
		// 该 channel 已被 close，select 立即返回并重查，不会漏
		sig := tree.ChangeSignal()
		now := time.Now().Unix()
		cutoff := max(tree.ChangeLogSettled(), start) // 覆盖点不回退（时钟回拨时）
		recentChanges, err := tree.GetChangedDirs(start, cutoff-1)
		covered := cutoff
		expired := !time.Now().Before(holdDeadline)
		if err != nil {
			// 查询失败不能当"区间内无变更"回：客户端会把游标推进到覆盖点，这段变更
			// 就只能等全量扫描补。继续挂起重试；挂满仍失败则回空列表且覆盖点留在
			// start，游标原地不动
			log.Error("Error getting changed dirs:", err)
			recentChanges = nil
			covered = start
		}

		if len(recentChanges) > 0 || expired {
			responseMsg := buildRecentChangeResponse(recentChanges, covered)
			if serr := sendMessage(conn, MsgTypeRecentChangeResponse, encodeRecentChangeResponse(responseMsg)); serr != nil {
				return fmt.Errorf("%w, error sending recent change response: %v", appError.ErrConnection, serr)
			}
			log.Debugf("Sent recent change response to %s, changes count: %d, fullResync=%v",
				conn.RemoteAddr().String(), len(responseMsg.Changes), responseMsg.FullResync)
			return nil
		}

		// 已有记录但还没定型（键 ≥ cutoff）：已提交的不会再有广播来叫醒，到下一秒
		// 边界再查，而不是挂满 LongPollHold（在途的提交完成后会广播）
		wait := time.Until(holdDeadline)
		if err == nil {
			if pending, perr := tree.GetChangedDirs(cutoff, now); perr == nil && len(pending) > 0 {
				wait = min(wait, time.Until(time.Unix(now+1, 0)))
			}
		}
		select {
		case <-sig:
		case <-time.After(wait):
		}
	}
}
