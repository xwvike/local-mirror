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
		"empty tasks":  {"tasks: []", "no tasks"},
		"bad mode":     {"tasks:\n  - mode: server\n    path: /tmp/x", "invalid mode"},
		"empty path":   {"tasks:\n  - mode: reality\n    path: \"\"", "path must not be empty"},
		"dup path":     {"tasks:\n  - mode: reality\n    path: /tmp/x\n  - name: y\n    mode: mirror\n    path: /tmp/x", "share the same path"},
		"dup name":     {"tasks:\n  - name: n\n    mode: reality\n    path: /tmp/x1\n  - name: n\n    mode: reality\n    path: /tmp/x2", "duplicate task name"},
		"bad loglevel": {"tasks:\n  - mode: reality\n    path: /tmp/x\n    loglevel: verbose", "invalid log level"},
		"bad yaml":     {"tasks: [<<<", "failed to parse YAML"},
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

// TestLoadMultiConfigDirectionFields 方向优先字段归一到内部 Mode/RealityIP
func TestLoadMultiConfigDirectionFields(t *testing.T) {
	cfg, err := LoadMultiConfig(writeYAML(t, `
tasks:
  - name: src
    send: true
    path: /tmp/da
  - name: sink
    receive: true
    connect: 10.0.0.5
    path: /tmp/db
    allow_delete: true
  - name: sinklisten
    receive: true
    listen: true
    path: /tmp/dc
  - name: relay
    send: true
    receive: true
    connect: 10.0.0.9
    path: /tmp/dd
`))
	if err != nil {
		t.Fatalf("LoadMultiConfig: %v", err)
	}
	src, sink, sinklisten, relay := cfg.Tasks[0], cfg.Tasks[1], cfg.Tasks[2], cfg.Tasks[3]
	if src.Mode != "reality" {
		t.Errorf("send → reality, got %q", src.Mode)
	}
	if sink.Mode != "mirror" || sink.RealityIP != "10.0.0.5" || !sink.AllowDelete {
		t.Errorf("receive+connect wrong: %+v", sink)
	}
	if sinklisten.Mode != "mirror" || !sinklisten.Listen || sinklisten.RealityIP != "" {
		t.Errorf("receive+listen wrong: %+v", sinklisten)
	}
	if relay.Mode != "relay" || relay.RealityIP != "10.0.0.9" {
		t.Errorf("send+receive → relay, got mode=%q ip=%q", relay.Mode, relay.RealityIP)
	}
}

// TestLoadMultiConfigDirectionErrors 方向字段的非法组合
func TestLoadMultiConfigDirectionErrors(t *testing.T) {
	cases := map[string]struct {
		yml     string
		wantSub string
	}{
		"mix vocab":      {"tasks:\n  - send: true\n    mode: reality\n    path: /tmp/x", "cannot be mixed"},
		"mix realityip":  {"tasks:\n  - receive: true\n    realityip: 1.2.3.4\n    path: /tmp/x", "cannot be mixed"},
		"connect+listen": {"tasks:\n  - receive: true\n    connect: 1.2.3.4\n    listen: true\n    path: /tmp/x", "mutually exclusive"},
		"no direction":   {"tasks:\n  - connect: 1.2.3.4\n    path: /tmp/x", "need a direction"},
		"nothing at all": {"tasks:\n  - path: /tmp/x", "specify a direction"},
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
