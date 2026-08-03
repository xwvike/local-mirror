package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCfg 写一份指向 syncRoot 的单任务配置，返回配置文件路径
func writeCfg(t *testing.T, cfgPath, syncRoot string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		t.Fatal(err)
	}
	body := "tasks:\n  - name: solo\n    send: true\n    path: " + syncRoot + "\n    secret: LEAKY\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// TestR1MissingConfigFails R1：显式指定的配置文件不存在必须直接失败，
// 且报错要点名路径。绝不静默回落到别的配置——那是最难排查的一类故障
func TestR1MissingConfigFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yml")
	_, err := LoadMultiConfig(missing)
	if err == nil {
		t.Fatal("配置文件不存在时应报错")
	}
	if !strings.Contains(err.Error(), "config file not found") {
		t.Errorf("应明确报「找不到配置」，实际: %v", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("报错应点名具体路径，实际: %v", err)
	}
}

// TestR4ConfigInsideSyncRootRejected R4：配置文件位于同步根内部必须拒绝。
// 同步根会被复制到对端，落在里面的配置连同明文 secret 会被一并镜像出去
func TestR4ConfigInsideSyncRootRejected(t *testing.T) {
	syncRoot := t.TempDir()
	cfgPath := writeCfg(t, filepath.Join(syncRoot, "config.yml"), syncRoot)

	_, err := LoadMultiConfig(cfgPath)
	if err == nil {
		t.Fatal("配置文件在同步根内部时应拒绝加载")
	}
	if !strings.Contains(err.Error(), "inside the sync root") {
		t.Errorf("应说明原因是「位于同步根内部」，实际: %v", err)
	}
}

// TestR4NestedConfigRejected 深层嵌套同样要拦——只查同级会漏掉最常见的
// 「配置放在同步目录的子目录里」
func TestR4NestedConfigRejected(t *testing.T) {
	syncRoot := t.TempDir()
	nested := filepath.Join(syncRoot, "deploy", "conf")
	cfgPath := writeCfg(t, filepath.Join(nested, "config.yml"), syncRoot)

	if _, err := LoadMultiConfig(cfgPath); err == nil {
		t.Fatal("同步根子目录里的配置文件同样应被拒绝")
	}
}

// TestR4SymlinkCannotBypass 用符号链接把配置"指到外面"不能绕过检查：
// 校验对象必须是解引用后的真实路径
func TestR4SymlinkCannotBypass(t *testing.T) {
	syncRoot := t.TempDir()
	realCfg := writeCfg(t, filepath.Join(syncRoot, "real.yml"), syncRoot)

	// 在同步根外面放一个软链指向根内的真实配置
	outside := t.TempDir()
	link := filepath.Join(outside, "config.yml")
	if err := os.Symlink(realCfg, link); err != nil {
		t.Skipf("本平台不支持创建符号链接: %v", err)
	}

	if _, err := LoadMultiConfig(link); err == nil {
		t.Fatal("经软链指向同步根内部的配置文件同样应被拒绝")
	}
}

// TestR4ConfigOutsideSyncRootAccepted 正例：配置在同步根之外应正常加载，
// 确认 R4 没有误伤常规布局
func TestR4ConfigOutsideSyncRootAccepted(t *testing.T) {
	syncRoot := t.TempDir()
	cfgPath := writeCfg(t, filepath.Join(t.TempDir(), "config.yml"), syncRoot)

	cfg, err := LoadMultiConfig(cfgPath)
	if err != nil {
		t.Fatalf("同步根之外的配置应正常加载: %v", err)
	}
	if len(cfg.Tasks) != 1 || cfg.Tasks[0].Name != "solo" {
		t.Fatalf("解析结果异常: %+v", cfg.Tasks)
	}
}

// TestR4SiblingPrefixNotConfused 前缀相同但不在内部的目录不能误判：
// /tmp/rootX 不在 /tmp/root 之内
func TestR4SiblingPrefixNotConfused(t *testing.T) {
	base := t.TempDir()
	syncRoot := filepath.Join(base, "root")
	sibling := filepath.Join(base, "rootX")
	for _, d := range []string{syncRoot, sibling} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := writeCfg(t, filepath.Join(sibling, "config.yml"), syncRoot)

	if _, err := LoadMultiConfig(cfgPath); err != nil {
		t.Fatalf("同前缀的兄弟目录不应被判为「在同步根内」: %v", err)
	}
}
