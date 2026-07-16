package appError

import (
	"errors"
)

var (
	ErrConnection = errors.New("connection error")

	// ErrDiskFull 镜像侧磁盘空间不足。与连接错误严格区分：连接完好，
	// 断连重连毫无意义——按"跳过该文件 + 明确告知用户"处理，
	// 空间恢复后由下轮变更推送或全量扫描自动补上
	ErrDiskFull = errors.New("磁盘空间不足")
)
