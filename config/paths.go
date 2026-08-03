package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// 配置文件的约定落点。见 docs/CONFIG_AND_SERVICE.md §P1。
//
// ⚠️ 这些**不是搜索路径**：local-mirror 不做配置自动发现，运行时只认显式 --config。
// 它们的唯一用途是让 `service install` 知道该把配置建在哪、
// 以及在生成的服务描述文件里写哪个路径——两端约定同一个常量，避免各写各的。
const (
	// AppDirName 各平台配置目录下属于本程序的那一层
	AppDirName = "local-mirror"
	// ConfigFileName 配置目录内的文件名。已被 AppDirName 那层目录空间化，
	// 故可以用通用的 config.yml（不像放在任意工作目录时那样容易撞名）
	ConfigFileName = "config.yml"
)

// brewPrefixes macOS 上系统级配置的候选前缀，按优先级排列：
// Apple Silicon 的 Homebrew 前缀在前，Intel / 手工安装在后。
// 只探一个会在换架构的机器上莫名找不到配置。变量而非常量，供测试替换
var brewPrefixes = []string{"/opt/homebrew", "/usr/local"}

// SystemConfigDir 返回系统级（服务用）配置目录。
//
// 各平台按自己的原生规范，不强行统一。
func SystemConfigDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("ProgramData")
		if base == "" {
			return "", fmt.Errorf("cannot locate the system config directory: %%ProgramData%% is not set")
		}
		return filepath.Join(base, AppDirName), nil
	case "darwin":
		// 落在已存在的那个 brew 前缀下；都不在则退回 /usr/local（手工安装的常规位置）
		for _, prefix := range brewPrefixes {
			if fi, err := os.Stat(prefix); err == nil && fi.IsDir() {
				return filepath.Join(prefix, "etc", AppDirName), nil
			}
		}
		return filepath.Join("/usr/local", "etc", AppDirName), nil
	default:
		return filepath.Join("/etc", AppDirName), nil
	}
}

// UserConfigDir 返回用户级配置目录。
//
// ⚠️ 刻意不使用 os.UserConfigDir()：它在 macOS 上返回
// ~/Library/Application Support，那是给 GUI 应用的位置。local-mirror 是
// Unix 风格的守护型 CLI，且 mac 与 Linux 保持同一套路径能让两端运维心智一致。
func UserConfigDir() (string, error) {
	if runtime.GOOS == "windows" {
		base := os.Getenv("AppData")
		if base == "" {
			return "", fmt.Errorf("cannot locate the user config directory: %%AppData%% is not set")
		}
		return filepath.Join(base, AppDirName), nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, AppDirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the user config directory: %w", err)
	}
	return filepath.Join(home, ".config", AppDirName), nil
}

// SystemConfigPath 系统级配置文件的完整路径
func SystemConfigPath() (string, error) {
	dir, err := SystemConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ConfigFileName), nil
}

// UserConfigPath 用户级配置文件的完整路径
func UserConfigPath() (string, error) {
	dir, err := UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ConfigFileName), nil
}
