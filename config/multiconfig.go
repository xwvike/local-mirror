package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// TaskConfig 多任务配置中的单个任务，字段与命令行旗子一一对应。
// 监督进程把它映射为子进程的 argv（secret 例外，走环境变量）。
//
// 方向优先字段（send/receive/connect/listen）与 CLI 的 --send/--receive/
// --connect/--listen 对齐，是文档化的写法；老的 mode/realityip 仍被解析
// 以兼容既有 yml，但不再出现在文档里，且不能与方向字段混用
type TaskConfig struct {
	Name string `yaml:"name"` // 实例别名（-a），缺省取 path 的 basename，须唯一
	Path string `yaml:"path"` // 同步工作目录（-p，必填）

	// 方向优先（文档化）
	Send    bool   `yaml:"send"`    // 本端是源：数据流出
	Receive bool   `yaml:"receive"` // 本端是汇：数据流入（send+receive = 中继）
	Connect string `yaml:"connect"` // 拨向对端 host[:port]；对端须在监听
	Listen  bool   `yaml:"listen"`  // 等对端拨入（汇监听格）

	// 遗留兼容（不文档化）：解析后归一到 Mode/RealityIP，与方向字段互斥
	Mode      string `yaml:"mode"`      // reality / mirror / relay
	RealityIP string `yaml:"realityip"` // 上游地址

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
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	var cfg MultiConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	if len(cfg.Tasks) == 0 {
		return nil, fmt.Errorf("no tasks in config (tasks is empty)")
	}

	seenPaths := make(map[string]string) // 绝对路径 → 任务名
	seenNames := make(map[string]bool)
	for i := range cfg.Tasks {
		t := &cfg.Tasks[i]
		applyDefaults(t, &cfg.Defaults)

		// 方向优先字段归一到内部 Mode/RealityIP（与方向字段互斥）
		if err := resolveTaskDirection(t, i+1); err != nil {
			return nil, err
		}
		if t.Mode == "" {
			return nil, fmt.Errorf("task %d: specify a direction (send and/or receive)", i+1)
		}
		if _, ok := ModeMap[t.Mode]; !ok {
			// 只有遗留 mode 字段写了非法值才会到这里（方向字段只产出合法 Mode）
			return nil, fmt.Errorf("task %d: invalid mode %q", i+1, t.Mode)
		}
		if t.Path == "" {
			return nil, fmt.Errorf("task %d: path must not be empty", i+1)
		}
		abs, err := filepath.Abs(t.Path)
		if err != nil {
			return nil, fmt.Errorf("task %d: cannot resolve path %q: %w", i+1, t.Path, err)
		}
		t.Path = abs
		// 同一目录不能被两个任务使用：会争抢 .local-mirror 的 bbolt 锁，
		// 子进程反复启动失败，必须在这里前置拒绝
		if prev, dup := seenPaths[abs]; dup {
			return nil, fmt.Errorf("tasks %q and %q share the same path %s", t.Name, prev, abs)
		}

		if t.Name == "" {
			t.Name = filepath.Base(abs)
		}
		if seenNames[t.Name] {
			return nil, fmt.Errorf("duplicate task name %q (name defaults to the path basename; set it explicitly to resolve)", t.Name)
		}
		seenPaths[abs] = t.Name
		seenNames[t.Name] = true

		if t.LogLevel != "" {
			switch t.LogLevel {
			case "debug", "info", "warn", "error":
			default:
				return nil, fmt.Errorf("task %q: invalid log level %q", t.Name, t.LogLevel)
			}
		}
	}
	return &cfg, nil
}

// resolveTaskDirection 把方向优先字段（send/receive/connect/listen）归一到内部
// 的 Mode/RealityIP，供后续计数、聚合展示与 argv 映射复用。语义与 CLI 的
// resolveDirection 对齐：两套词汇不可混用；未给方向即报错。
func resolveTaskDirection(t *TaskConfig, n int) error {
	hasDir := t.Send || t.Receive || t.Connect != "" || t.Listen
	hasLegacy := t.Mode != "" || t.RealityIP != ""
	if !hasDir {
		return nil // 老词汇：Mode/RealityIP 原样交给下游校验
	}
	if hasLegacy {
		return fmt.Errorf("task %d: direction fields (send/receive/connect/listen) cannot be mixed with mode/realityip", n)
	}
	if t.Connect != "" && t.Listen {
		return fmt.Errorf("task %d: connect and listen are mutually exclusive on one link", n)
	}
	switch {
	case t.Send && t.Receive:
		t.Mode = "relay"
	case t.Send:
		t.Mode = "reality"
	case t.Receive:
		t.Mode = "mirror"
	default:
		return fmt.Errorf("task %d: connect/listen need a direction: add send (source) or receive (sink)", n)
	}
	t.RealityIP = t.Connect
	return nil
}

// applyDefaults 任务字段为零值时回退到 defaults 的同名字段。
// name/direction/path 是任务身份，不参与回退
func applyDefaults(t, d *TaskConfig) {
	if t.Connect == "" {
		t.Connect = d.Connect
	}
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
