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
func buildRecentChangeResponse(changes []string, now int64) RecentChangeResponseMessage {
	unique := utils.UniqueStrings(changes)
	if len(unique) > changeFullResyncThreshold {
		return RecentChangeResponseMessage{
			ServerID:     config.InstanceID,
			CoveredUntil: now,
			FullResync:   true,
		}
	}
	return RecentChangeResponseMessage{
		ServerID:     config.InstanceID,
		CoveredUntil: now,
		Changes:      unique,
	}
}

func (s *fileServer) handleRecentChangeRequest(ID uint32, bodyBytes []byte) error {
	_client, ok := s.clientMap.Load(ID)
	if !ok {
		// 与其余 handler 一致：未握手/已注销的连接按连接错误关闭，
		// 而不是静默不应答让对端干等读超时
		return fmt.Errorf("%w, client not found for ID: %d", appError.ErrConnection, ID)
	}
	conn := _client.(*client).Conn
	recentChangeRequest, err := decodeRecentChangeRequest(bodyBytes)
	if err != nil {
		return fmt.Errorf("%w, error decoding recent change request: %v", appError.ErrConnection, err)
	}
	log.Debugf("Received recent change request from %s, client ID: %d, startTime: %d",
		conn.RemoteAddr().String(), recentChangeRequest.ClientID, recentChangeRequest.StartTime)

	// 长轮询：区间内已有变更立刻回（追赶/重连场景）；无变更则挂起，
	// 等到变更落库广播或挂满上限后返回。挂起期间不读 socket，
	// 上限兜底避免死连接常驻。上界用服务端当前时刻，随每次唤醒重新求值。
	start := recentChangeRequest.StartTime
	holdDeadline := time.Now().Add(LongPollHold)
	for {
		// 先取信号再查询：若广播发生在查询之后、select 之前，
		// 该 channel 已被 close，select 立即返回并重查，不会漏
		sig := tree.ChangeSignal()
		now := time.Now().Unix()
		recentChanges, err := tree.GetChangedDirs(start, now)
		if err != nil {
			log.Error("Error getting changed dirs:", err)
			recentChanges = nil
		}

		if len(recentChanges) > 0 || err != nil || !time.Now().Before(holdDeadline) {
			responseMsg := buildRecentChangeResponse(recentChanges, now)
			if serr := sendMessage(conn, MsgTypeRecentChangeResponse, encodeRecentChangeResponse(responseMsg)); serr != nil {
				return fmt.Errorf("%w, error sending recent change response: %v", appError.ErrConnection, serr)
			}
			log.Debugf("Sent recent change response to %s, changes count: %d, fullResync=%v",
				conn.RemoteAddr().String(), len(responseMsg.Changes), responseMsg.FullResync)
			return nil
		}

		select {
		case <-sig:
		case <-time.After(time.Until(holdDeadline)):
		}
	}
}
