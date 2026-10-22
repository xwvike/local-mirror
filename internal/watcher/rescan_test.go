package watcher

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
)

// TestRescanErrorIsOverflow FSEvents 的丢事件与 inotify 的队列溢出走同一条处理路径
func TestRescanErrorIsOverflow(t *testing.T) {
	if !errors.Is(&rescanError{Path: "/x"}, fsnotify.ErrEventOverflow) {
		t.Error("rescanError must match fsnotify.ErrEventOverflow")
	}
}

func TestInRescanScope(t *testing.T) {
	sub := filepath.Join("a", "b")
	cases := []struct {
		dir    string
		scopes []string
		want   bool
	}{
		{"x", []string{"."}, true},
		{"a", []string{"a"}, true},
		{sub, []string{"a"}, true},
		{"ab", []string{"a"}, false}, // 前缀相同但不是子目录
		{"a", []string{sub}, false},
		{"c", []string{"a", "c"}, true},
	}
	for _, c := range cases {
		scopes := map[string]struct{}{}
		for _, s := range c.scopes {
			scopes[s] = struct{}{}
		}
		if got := inRescanScope(c.dir, scopes); got != c.want {
			t.Errorf("inRescanScope(%q, %v) = %v, want %v", c.dir, c.scopes, got, c.want)
		}
	}
}
