package utils

import "testing"

func TestIsIgnored(t *testing.T) {
	patterns := []string{"node_modules", "*.log", "cache-*", ".DS_Store", "v[12]"}
	cases := []struct {
		path string
		want bool
	}{
		// 纯名字精确匹配（任意深度）
		{"node_modules", true},
		{"a/node_modules/b.js", true},
		{"src/index.js", false},
		// 不误伤：段匹配而非子串
		{"cabinet/file.txt", false},        // "bin" 不在列表，即便在也不该命中 cabinet
		{"my_node_modules/x", false},       // 前缀相似不命中
		{"node_modules_backup/x", false},   // 后缀相似不命中
		{"deep/nested/node_modules", true}, // 深层命中
		// * 通配符
		{"debug.log", true},
		{"logs/app.log", true},
		{"app.log.bak", false}, // *.log 不匹配 .log.bak 段
		{"cache-tmp/file", true},
		{"cache/file", false}, // cache-* 要求前缀后有内容? filepath.Match("cache-*","cache") = false
		// ? 与 [] 类
		{"v1/file", true},
		{"v2/file", true},
		{"v3/file", false},
		// 大小写敏感
		{"Node_Modules/x", false},
		{"DEBUG.LOG", false},
		// 边界
		{".", false},
		{".DS_Store", true},
	}
	for _, c := range cases {
		if got := IsIgnored(c.path, patterns); got != c.want {
			t.Errorf("IsIgnored(%q) = %v, want %v", c.path, got, c.want)
		}
	}
	// 空列表永不命中
	if IsIgnored("anything", nil) {
		t.Error("empty pattern list matched")
	}
}
