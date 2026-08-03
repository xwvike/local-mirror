package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestUserConfigDirIsDotConfig 用户级目录刻意用 ~/.config 而非
// os.UserConfigDir()——后者在 macOS 上给的是 ~/Library/Application Support，
// 那是 GUI 应用的位置，且会让 mac 与 Linux 的运维路径分裂
func TestUserConfigDirIsDotConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows 走 %AppData%，另行覆盖")
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	dir, err := UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(dir, filepath.Join(".config", AppDirName)) {
		t.Errorf("期望以 .config/%s 结尾，实际 %s", AppDirName, dir)
	}
	if strings.Contains(dir, "Application Support") {
		t.Errorf("不应落到 macOS 的 GUI 应用目录: %s", dir)
	}
}

// TestUserConfigDirHonorsXDG Linux 的 XDG 规范优先于 ~/.config
func TestUserConfigDirHonorsXDG(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG 不适用于 windows")
	}
	t.Setenv("XDG_CONFIG_HOME", "/custom/xdg")
	dir, err := UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/custom/xdg", AppDirName); dir != want {
		t.Errorf("期望 %s，实际 %s", want, dir)
	}
}

// TestSystemConfigDirDarwinPrefersExistingBrewPrefix macOS 上按优先级挑
// 已存在的 brew 前缀：Apple Silicon 的 /opt/homebrew 在前、Intel 的 /usr/local 在后。
// 只探一个会在换架构的机器上莫名找不到配置
func TestSystemConfigDirDarwinPrefersExistingBrewPrefix(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("仅 darwin 有 brew 前缀分歧")
	}
	base := t.TempDir()
	armPrefix := filepath.Join(base, "opt-homebrew")
	intelPrefix := filepath.Join(base, "usr-local")

	// 只有 Intel 前缀存在 → 应选它
	if err := os.MkdirAll(intelPrefix, 0755); err != nil {
		t.Fatal(err)
	}
	orig := brewPrefixes
	t.Cleanup(func() { brewPrefixes = orig })
	brewPrefixes = []string{armPrefix, intelPrefix}

	dir, err := SystemConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(intelPrefix, "etc", AppDirName); dir != want {
		t.Errorf("只有 Intel 前缀存在时应选它：期望 %s，实际 %s", want, dir)
	}

	// 两个都存在 → 应优先 ARM
	if err := os.MkdirAll(armPrefix, 0755); err != nil {
		t.Fatal(err)
	}
	dir, err = SystemConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(armPrefix, "etc", AppDirName); dir != want {
		t.Errorf("两者都在时应优先 ARM 前缀：期望 %s，实际 %s", want, dir)
	}
}

// TestConfigPathsJoinFileName 目录与完整路径保持一致，避免两处各拼各的
func TestConfigPathsJoinFileName(t *testing.T) {
	dir, err := SystemConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	path, err := SystemConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, ConfigFileName); path != want {
		t.Errorf("期望 %s，实际 %s", want, path)
	}
}
