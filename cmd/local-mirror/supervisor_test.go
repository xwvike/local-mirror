package main

import (
	"strings"
	"testing"

	"local-mirror/config"
)

// argvString 把 argv 拼成一个便于子串断言的字符串
func argvString(args []string) string { return strings.Join(args, " ") }

// TestTaskArgsDirectionFirst taskArgs 输出方向优先词汇（--send/--receive/
// --connect/--listen），不再出现遗留的 -m/-r——ps 里也干净
func TestTaskArgsDirectionFirst(t *testing.T) {
	cases := []struct {
		name string
		t    config.TaskConfig
		want []string
		deny []string
	}{
		{
			name: "source listens (classic)",
			t:    config.TaskConfig{Mode: "reality", Path: "/srv/a", Name: "a"},
			want: []string{"--send", "-p", "/srv/a"},
			deny: []string{"-m", "-r", "--connect", "--listen"},
		},
		{
			name: "sink dials",
			t:    config.TaskConfig{Mode: "mirror", Path: "/srv/b", Name: "b", RealityIP: "10.0.0.5"},
			want: []string{"--receive", "--connect", "10.0.0.5"},
			deny: []string{"-m", "-r", "--listen"},
		},
		{
			name: "sink listens",
			t:    config.TaskConfig{Mode: "mirror", Path: "/srv/c", Name: "c", Listen: true},
			want: []string{"--receive", "--listen"},
			deny: []string{"-m", "-r", "--connect"},
		},
		{
			name: "source dials",
			t:    config.TaskConfig{Mode: "reality", Path: "/srv/d", Name: "d", RealityIP: "vps:52345"},
			want: []string{"--send", "--connect", "vps:52345"},
			deny: []string{"-m", "-r", "--receive"},
		},
		{
			name: "relay",
			t:    config.TaskConfig{Mode: "relay", Path: "/srv/e", Name: "e", RealityIP: "10.0.0.9"},
			want: []string{"--send", "--receive", "--connect", "10.0.0.9"},
			deny: []string{"-m", "-r"},
		},
	}
	for _, c := range cases {
		got := argvString(taskArgs(c.t))
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: argv %q missing %q", c.name, got, w)
			}
		}
		for _, d := range c.deny {
			// 用空格包裹避免 --connect 里的子串 -r 误伤
			if strings.Contains(" "+got+" ", " "+d+" ") {
				t.Errorf("%s: argv %q should not contain %q", c.name, got, d)
			}
		}
	}
}
