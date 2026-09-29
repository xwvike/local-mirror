package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"local-mirror/config"
)

// upsertTaskYAML 改写的是用户的配置：已有任务与注释不能丢，同一同步根重装不能出现重复任务
func TestUpsertTaskYAML(t *testing.T) {
	cfgPath := "/etc/local-mirror/config.yml"
	parse := func(t *testing.T, data []byte) *config.MultiConfig {
		t.Helper()
		cfg, err := config.ParseMultiConfig(data, cfgPath)
		if err != nil {
			t.Fatalf("result is not a valid config: %v\n%s", err, data)
		}
		return cfg
	}
	backup := config.TaskConfig{Receive: true, Listen: true, Path: "/srv/backup", AllowDelete: true, Secret: "k1"}

	// 空白模板（纯注释）：注释原样保留，末尾追加 tasks
	out, err := upsertTaskYAML([]byte(blankConfigTemplate), backup)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), blankConfigTemplate) {
		t.Error("blank template comments were not kept verbatim")
	}
	if cfg := parse(t, out); len(cfg.Tasks) != 1 || cfg.Tasks[0].Path != "/srv/backup" || !cfg.Tasks[0].Listen {
		t.Errorf("unexpected tasks after first install: %+v", cfg.Tasks)
	}

	// 已有带注释的配置：另一个同步根 → 追加，原任务与注释都在
	existing := []byte("# my config\ndefaults:\n  loglevel: info\ntasks:\n  # photos share\n  - name: photos\n    send: true\n    path: /srv/photos\n    secret: k0\n")
	out, err = upsertTaskYAML(existing, backup)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"# my config", "# photos share"} {
		if !strings.Contains(string(out), c) {
			t.Errorf("comment %q lost", c)
		}
	}
	cfg := parse(t, out)
	if len(cfg.Tasks) != 2 || cfg.Tasks[0].Name != "photos" || cfg.Tasks[1].Path != "/srv/backup" {
		t.Errorf("expected photos + backup, got %+v", cfg.Tasks)
	}

	// 同一同步根再装一次（参数变了）→ 就地替换，不重复
	changed := backup
	changed.AllowDelete = false
	changed.Secret = "k2"
	out, err = upsertTaskYAML(out, changed)
	if err != nil {
		t.Fatal(err)
	}
	cfg = parse(t, out)
	if len(cfg.Tasks) != 2 {
		t.Fatalf("reinstall on the same sync root duplicated the task: %+v", cfg.Tasks)
	}
	if got := cfg.Tasks[1]; got.AllowDelete || got.Secret != "k2" {
		t.Errorf("same-root task not replaced: %+v", got)
	}
	if !strings.Contains(string(out), "# photos share") {
		t.Error("comment lost after replacing a task")
	}
}

// removeTaskYAML 只删指定同步根的任务：其余任务与注释都在；不存在的目录不改动
func TestRemoveTaskYAML(t *testing.T) {
	existing := []byte("# my config\ntasks:\n  # photos share\n  - name: photos\n    send: true\n    path: /srv/photos\n    secret: k0\n  - receive: true\n    listen: true\n    path: /srv/backup\n    secret: k1\n")

	out, removed, left, err := removeTaskYAML(existing, "/srv/nope")
	if err != nil || removed || string(out) != string(existing) {
		t.Fatalf("removing an unknown dir must change nothing: removed=%v err=%v", removed, err)
	}

	out, removed, left, err = removeTaskYAML(existing, "/srv/backup")
	if err != nil || !removed || left != 1 {
		t.Fatalf("removed=%v left=%d err=%v", removed, left, err)
	}
	cfg, err := config.ParseMultiConfig(out, "/etc/local-mirror/config.yml")
	if err != nil || len(cfg.Tasks) != 1 || cfg.Tasks[0].Name != "photos" {
		t.Fatalf("expected only photos left: %v %+v", err, cfg)
	}
	for _, c := range []string{"# my config", "# photos share"} {
		if !strings.Contains(string(out), c) {
			t.Errorf("comment %q lost", c)
		}
	}

	// 删掉最后一个：剩下的配置是"还没有任务"，不是"有错"
	out, removed, left, err = removeTaskYAML(out, "/srv/photos")
	if err != nil || !removed || left != 0 {
		t.Fatalf("removed=%v left=%d err=%v", removed, left, err)
	}
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, out, 0600); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := inspectConfig(path); state != cfgBlank {
		t.Errorf("config with its last task removed should be blank, got state %d", state)
	}
}

// 写错的配置绝不能被当成"还没填"：restart 与 install 据此拒绝，并把错误摆出来
func TestInspectConfigBrokenIsNotBlank(t *testing.T) {
	dir, root := t.TempDir(), t.TempDir() // 配置不得落在任务同步根内，分开放
	cases := []struct {
		name, content string
		want          configState
	}{
		{"blank template", blankConfigTemplate, cfgBlank},
		{"empty task list", "tasks: []\n", cfgBlank},
		{"valid", "tasks:\n  - send: true\n    path: " + root + "\n    secret: k\n", cfgReady},
		{"typo in a key", "tasks:\n  - send: true\n    path: " + root + "\n    secrett: k\n", cfgBroken},
		{"yaml syntax error", "tasks:\n  - send: true\n   path: [\n", cfgBroken},
	}
	for _, c := range cases {
		path := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "_")+".yml")
		if err := os.WriteFile(path, []byte(c.content), 0600); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := inspectConfig(path); got != c.want {
			t.Errorf("%s: state %d, want %d", c.name, got, c.want)
		}
	}
	if got, _, _ := inspectConfig(filepath.Join(dir, "absent.yml")); got != cfgMissing {
		t.Errorf("absent file: state %d, want missing", got)
	}
}
