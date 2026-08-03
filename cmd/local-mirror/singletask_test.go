package main

import (
	"strings"
	"testing"

	"local-mirror/config"
)

// TestApplySingleTaskMapsEveryField 单任务免 fork 路径必须把 TaskConfig 的每个字段
// 都落进本进程的旗子状态——它走的是 taskArgs 进程内重解析，等价于命令行直接给旗子。
// 少落一个字段就意味着「配置写了但不生效」，是最难发现的一类故障
func TestApplySingleTaskMapsEveryField(t *testing.T) {
	// applySingleTask 会改全局旗子状态，跑完还原，避免污染同包其他用例
	restore := map[string]any{
		"path": *config.Path, "alias": *config.Alias, "loglevel": *config.LogLevel,
		"secret": *config.Secret, "mode": *config.Mode, "ignore": *config.Ignore,
		"allowDelete": *config.AllowDelete, "allowCritical": *config.AllowCritical,
		"cooldown": *config.CoolDown, "fileBuf": *config.FileBufferSize,
		"listen": *config.ListenFlag, "send": *config.SendFlag, "receive": *config.ReceiveFlag,
		"connect": *config.ConnectTo,
	}
	t.Cleanup(func() {
		*config.Path = restore["path"].(string)
		*config.Alias = restore["alias"].(string)
		*config.LogLevel = restore["loglevel"].(string)
		*config.Secret = restore["secret"].(string)
		*config.Mode = restore["mode"].(string)
		*config.Ignore = restore["ignore"].(string)
		*config.AllowDelete = restore["allowDelete"].(bool)
		*config.AllowCritical = restore["allowCritical"].(bool)
		*config.CoolDown = restore["cooldown"].(int64)
		*config.FileBufferSize = restore["fileBuf"].(uint64)
		*config.ListenFlag = restore["listen"].(bool)
		*config.SendFlag = restore["send"].(bool)
		*config.ReceiveFlag = restore["receive"].(bool)
		*config.ConnectTo = restore["connect"].(string)
	})

	const secret = "single-task-secret"
	task := config.TaskConfig{
		Name: "solo", Path: "/srv/solo", Mode: "mirror",
		RealityIP: "10.0.0.9", Listen: false,
		Ignore: []string{"cache", "*.log"}, Secret: secret,
		LogLevel: "warn", AllowDelete: true, AllowCritical: true,
		CoolDown: 3600, FileBufferSize: 128 * 1024,
	}
	applySingleTask(task)

	if *config.Path != "/srv/solo" {
		t.Errorf("path 未落地: %q", *config.Path)
	}
	if *config.Alias != "solo" {
		t.Errorf("alias 未落地: %q", *config.Alias)
	}
	if *config.LogLevel != "warn" {
		t.Errorf("loglevel 未落地: %q", *config.LogLevel)
	}
	if *config.ConnectTo != "10.0.0.9" {
		t.Errorf("connect 未落地: %q", *config.ConnectTo)
	}
	if *config.Ignore != "cache,*.log" {
		t.Errorf("ignore 未落地: %q", *config.Ignore)
	}
	if !*config.AllowDelete || !*config.AllowCritical {
		t.Errorf("allow_delete/allow_critical 未落地: %v/%v", *config.AllowDelete, *config.AllowCritical)
	}
	if *config.CoolDown != 3600 {
		t.Errorf("cooldown 未落地: %d", *config.CoolDown)
	}
	if *config.FileBufferSize != 128*1024 {
		t.Errorf("filebuffersize 未落地: %d", *config.FileBufferSize)
	}
	if !*config.ReceiveFlag {
		t.Errorf("mirror 应映射为 --receive，实际 receive=%v", *config.ReceiveFlag)
	}
	// 口令走独立赋值，绝不进 argv（ps 可见）
	if *config.Secret != secret {
		t.Errorf("secret 未落地: %q", *config.Secret)
	}
	if got := argvString(taskArgs(task)); strings.Contains(got, secret) {
		t.Errorf("secret 泄漏进 argv: %s", got)
	}
}
