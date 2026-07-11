package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// TaskConfig 多任务配置中的单个任务，字段与命令行旗子一一对应。
// 监督进程把它映射为子进程的 argv（secret 例外，走环境变量）
type TaskConfig struct {
	Name           string   `yaml:"name"`           // 实例别名（-a），缺省取 path 的 basename，须唯一
	Mode           string   `yaml:"mode"`           // reality / mirror / relay（必填）
	Path           string   `yaml:"path"`           // 同步根目录（-p，必填）
	RealityIP      string   `yaml:"realityip"`      // 上游地址（-r，mirror/relay）
	Ignore         []string `yaml:"ignore"`         // 忽略模式（-i）
	Secret         string   `yaml:"secret"`         // 传输加密口令（经环境变量传递，不进 argv）
	LogLevel       string   `yaml:"loglevel"`       // 日志级别（-l）
	AllowDelete    bool     `yaml:"allow_delete"`   // 删除同步（--allow-delete）
	AllowCritical  bool     `yaml:"allow_critical"` // 允许在关键路径上同步（--allow-critical）
	CoolDown       int64    `yaml:"cooldown"`       // 全量扫描间隔（-c）
	FileBufferSize uint64   `yaml:"filebuffersize"` // 传输分块（-f）
}

// MultiConfig --config 指定的 YAML 顶层结构
type MultiConfig struct {
	// Defaults 各任务字段留空（零值）时的回退值；name/mode/path 不参与回退
	Defaults TaskConfig   `yaml:"defaults"`
	Tasks    []TaskConfig `yaml:"tasks"`
}

// LoadMultiConfig 读取并校验多任务 YAML 配置。
// 返回的任务已完成 defaults 合并与 name 缺省填充；
// 任何校验失败返回错误（调用方按用法错误处理，exit 2）
func LoadMultiConfig(path string) (*MultiConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	var cfg MultiConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 YAML 失败: %w", err)
	}
	if len(cfg.Tasks) == 0 {
		return nil, fmt.Errorf("配置中没有任务（tasks 为空）")
	}

	seenPaths := make(map[string]string) // 绝对路径 → 任务名
	seenNames := make(map[string]bool)
	for i := range cfg.Tasks {
		t := &cfg.Tasks[i]
		applyDefaults(t, &cfg.Defaults)

		if _, ok := ModeMap[t.Mode]; !ok {
			return nil, fmt.Errorf("任务 %d: 无效的运行模式 %q (可选: reality, mirror, relay)", i+1, t.Mode)
		}
		if t.Path == "" {
			return nil, fmt.Errorf("任务 %d: path 不能为空", i+1)
		}
		abs, err := filepath.Abs(t.Path)
		if err != nil {
			return nil, fmt.Errorf("任务 %d: 无法解析路径 %q: %w", i+1, t.Path, err)
		}
		t.Path = abs
		// 同一目录不能被两个任务使用：会争抢 .local-mirror 的 bbolt 锁，
		// 子进程反复启动失败，必须在这里前置拒绝
		if prev, dup := seenPaths[abs]; dup {
			return nil, fmt.Errorf("任务 %q 与 %q 使用了同一路径 %s", t.Name, prev, abs)
		}

		if t.Name == "" {
			t.Name = filepath.Base(abs)
		}
		if seenNames[t.Name] {
			return nil, fmt.Errorf("任务名 %q 重复（name 缺省取路径末段，冲突时请显式命名）", t.Name)
		}
		seenPaths[abs] = t.Name
		seenNames[t.Name] = true

		if t.LogLevel != "" {
			switch t.LogLevel {
			case "debug", "info", "warn", "error":
			default:
				return nil, fmt.Errorf("任务 %q: 无效的日志级别 %q", t.Name, t.LogLevel)
			}
		}
	}
	return &cfg, nil
}

// applyDefaults 任务字段为零值时回退到 defaults 的同名字段。
// name/mode/path 是任务身份，不参与回退
func applyDefaults(t, d *TaskConfig) {
	if t.RealityIP == "" {
		t.RealityIP = d.RealityIP
	}
	if len(t.Ignore) == 0 {
		t.Ignore = d.Ignore
	}
	if t.Secret == "" {
		t.Secret = d.Secret
	}
	if t.LogLevel == "" {
		t.LogLevel = d.LogLevel
	}
	if !t.AllowDelete {
		t.AllowDelete = d.AllowDelete
	}
	if !t.AllowCritical {
		t.AllowCritical = d.AllowCritical
	}
	if t.CoolDown == 0 {
		t.CoolDown = d.CoolDown
	}
	if t.FileBufferSize == 0 {
		t.FileBufferSize = d.FileBufferSize
	}
}
