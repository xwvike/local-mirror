package safety

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckSyncSafetyNonCritical(t *testing.T) {
	// 普通临时目录不是关键路径：无限制、不快照
	snap, err := CheckSyncSafety(t.TempDir(), false)
	if err != nil {
		t.Fatalf("非关键路径被拒: %v", err)
	}
	if snap {
		t.Error("非关键路径不应开启快照")
	}
}

func TestCheckSyncSafetyCritical(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("无法确定 home 目录")
	}
	// 家目录是关键路径：未解锁必拒
	if _, err := CheckSyncSafety(home, false); err == nil {
		t.Error("关键路径未解锁却放行")
	}
	// 解锁后放行并开启快照
	snap, err := CheckSyncSafety(home, true)
	if err != nil {
		t.Fatalf("--allow-critical 后仍被拒: %v", err)
	}
	if !snap {
		t.Error("关键路径解锁后应开启快照")
	}
}

func TestIsCriticalRoot(t *testing.T) {
	if ok, _ := IsCriticalRoot(t.TempDir()); ok {
		t.Error("临时目录被误判为关键路径")
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if ok, _ := IsCriticalRoot(home); !ok {
			t.Error("家目录应判为关键路径")
		}
	}
}

func TestSnapshotBeforeOverwrite(t *testing.T) {
	root := t.TempDir()
	rel := "sub/config.conf"
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("原始内容"), 0644); err != nil {
		t.Fatal(err)
	}

	// 首次覆盖前快照
	if err := SnapshotBeforeOverwrite(root, rel, full); err != nil {
		t.Fatalf("SnapshotBeforeOverwrite: %v", err)
	}
	backup := filepath.Join(root, ".local-mirror", "backups", rel)
	got, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("快照未生成: %v", err)
	}
	if string(got) != "原始内容" {
		t.Errorf("快照内容 %q != 原始内容", got)
	}

	// 模拟覆盖：改动原文件后二次调用，快照必须仍是最初的原始版本（不 churn）
	if err := os.WriteFile(full, []byte("第二版"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := SnapshotBeforeOverwrite(root, rel, full); err != nil {
		t.Fatal(err)
	}
	got2, _ := os.ReadFile(backup)
	if string(got2) != "原始内容" {
		t.Errorf("二次调用 churn 了快照: %q", got2)
	}

	// 新文件（目标不存在）→ 不建快照
	newRel := "brand-new.txt"
	if err := SnapshotBeforeOverwrite(root, newRel, filepath.Join(root, newRel)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".local-mirror", "backups", newRel)); err == nil {
		t.Error("不存在的目标不该产生快照")
	}
}
