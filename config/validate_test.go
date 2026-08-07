package config

import "testing"

// TestValidateRuntimeNumbers 验证 CFG-01 的数值域校验：-f 0 / 过大、-c 0 / 负数都被拒，
// 默认值合法。这是直连 CLI / 单任务 / 多任务子进程共用的启动闸门。
func TestValidateRuntimeNumbers(t *testing.T) {
	saveBuf, saveCd := FileBufferSize, CoolDown
	defer func() { FileBufferSize, CoolDown = saveBuf, saveCd }()
	set := func(buf uint64, cd int64) {
		b, c := buf, cd
		FileBufferSize, CoolDown = &b, &c
	}

	set(64*1024, 1800)
	if err := ValidateRuntimeNumbers(); err != nil {
		t.Errorf("默认值应合法: %v", err)
	}
	set(0, 1800)
	if err := ValidateRuntimeNumbers(); err == nil {
		t.Error("filebuffersize=0 应被拒（会让发送循环空转）")
	}
	set(MaxFileBufferSize+1, 1800)
	if err := ValidateRuntimeNumbers(); err == nil {
		t.Error("filebuffersize 超上限应被拒")
	}
	set(64*1024, 0)
	if err := ValidateRuntimeNumbers(); err == nil {
		t.Error("cooldown=0 应被拒（会退化成每轮全量扫描）")
	}
	set(64*1024, -1)
	if err := ValidateRuntimeNumbers(); err == nil {
		t.Error("cooldown 负数应被拒")
	}
}
