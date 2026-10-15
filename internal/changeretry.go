package app

import (
	"sort"

	log "github.com/sirupsen/logrus"
)

// 变更追踪的目录级重试。
//
// 变更追踪按服务端时钟推进游标：一轮处理完就把游标推到 coveredUntil，服务端
// 日志里早于它的变更不会再被查到。若这一轮里某个目录没处理成功（连接闪断后
// 立即重连成功、目录里有文件下载失败、下钻的子目录重试耗尽……），它的变更就
// 被游标跨过去了，只能等下一次全量扫描（默认 --cooldown 1800 秒）才补上——
// 这半小时里汇端缺着这些文件，而日志只有一行错误。
//
// 这里把失败的目录记下来，下一轮变更追踪（长轮询返回时，最长约 LongPollHold）
// 与新变更一起重新处理。同一目录连续失败 maxChangeRetries 次后放手，交给全量
// 扫描兜底：持续性的本地错误不该每分钟都重试一遍。
//
// 所有任务由 taskMutex 串行执行，这里的状态无需加锁。
const maxChangeRetries = 3

var (
	retryDirs  = map[string]int{} // 待重试目录 → 已连续失败次数
	retryPrior map[string]int     // 本轮开始时的失败次数，noteRetry 据此累加
)

// beginRetryRound 开始一轮变更追踪：取出待重试的目录（按路径排序，结果稳定），
// 并清空登记表——本轮再次失败的目录由 noteRetry 重新登记，成功的自然消失
func beginRetryRound() []string {
	retryPrior, retryDirs = retryDirs, map[string]int{}
	due := make([]string, 0, len(retryPrior))
	for d := range retryPrior {
		due = append(due, d)
	}
	sort.Strings(due)
	return due
}

// noteRetry 登记一个本轮没处理成功的目录，下一轮变更追踪重试它
func noteRetry(dir string) {
	if _, already := retryDirs[dir]; already {
		return
	}
	n := retryPrior[dir] + 1
	if n > maxChangeRetries {
		log.Warnf("%s still failing after %d change-tracking retries; leaving it to the next full scan", dir, n-1)
		return
	}
	retryDirs[dir] = n
}

// resetRetries 全量扫描覆盖整棵树，之前登记的重试随之作废
func resetRetries() {
	retryDirs, retryPrior = map[string]int{}, nil
}
