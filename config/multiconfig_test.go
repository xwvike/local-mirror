package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.yml")
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMultiConfigMergeDefaults(t *testing.T) {
	cfg, err := LoadMultiConfig(writeYAML(t, `
defaults:
  loglevel: info
  secret: shared
  ignore: [node_modules]
tasks:
  - mode: reality
    path: /tmp/a
  - name: custom
    mode: mirror
    path: /tmp/b
    realityip: 10.0.0.5
    loglevel: warn
    secret: own
    ignore: ["*.log", dist]
    allow_delete: true
`))
	if err != nil {
		t.Fatalf("LoadMultiConfig: %v", err)
	}
	a, b := cfg.Tasks[0], cfg.Tasks[1]

	// 任务 1 继承 defaults；name 缺省取 basename
	if a.Name != "a" || a.LogLevel != "info" || a.Secret != "shared" ||
		!slices.Equal(a.Ignore, []string{"node_modules"}) {
		t.Errorf("task a defaults merge wrong: %+v", a)
	}
	// 任务 2 自有字段覆盖 defaults
	if b.Name != "custom" || b.LogLevel != "warn" || b.Secret != "own" ||
		!slices.Equal(b.Ignore, []string{"*.log", "dist"}) || !b.AllowDelete {
		t.Errorf("task b override wrong: %+v", b)
	}
	if !filepath.IsAbs(a.Path) || !filepath.IsAbs(b.Path) {
		t.Error("paths not absolutized")
	}
}

func TestLoadMultiConfigErrors(t *testing.T) {
	cases := map[string]struct {
		yml     string
		wantSub string
	}{
		"empty tasks":  {"tasks: []", "没有任务"},
		"bad mode":     {"tasks:\n  - mode: server\n    path: /tmp/x", "无效的运行模式"},
		"empty path":   {"tasks:\n  - mode: reality\n    path: \"\"", "path 不能为空"},
		"dup path":     {"tasks:\n  - mode: reality\n    path: /tmp/x\n  - name: y\n    mode: mirror\n    path: /tmp/x", "同一路径"},
		"dup name":     {"tasks:\n  - name: n\n    mode: reality\n    path: /tmp/x1\n  - name: n\n    mode: reality\n    path: /tmp/x2", "重复"},
		"bad loglevel": {"tasks:\n  - mode: reality\n    path: /tmp/x\n    loglevel: verbose", "无效的日志级别"},
		"bad yaml":     {"tasks: [<<<", "解析 YAML 失败"},
	}
	for name, c := range cases {
		_, err := LoadMultiConfig(writeYAML(t, c.yml))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("%s: error %q missing %q", name, err, c.wantSub)
		}
	}
}

func TestLoadMultiConfigDupPathRelativeAbsolute(t *testing.T) {
	// 相对与绝对写法指向同一目录也要拒绝
	wd, _ := os.Getwd()
	yml := "tasks:\n  - mode: reality\n    path: " + filepath.Join(wd, "x") + "\n  - name: two\n    mode: reality\n    path: x"
	if _, err := LoadMultiConfig(writeYAML(t, yml)); err == nil {
		t.Error("relative/absolute same-dir duplicate accepted")
	}
}
