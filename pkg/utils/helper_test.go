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

// TestIsIgnoredPathPatterns 路径锚定模式：含 "/" 的模式从同步根锚定，
// 不会像单段名字那样误伤任意深度的同名目录
func TestIsIgnoredPathPatterns(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
		why     string
	}{
		// 根锚定：只命中那一棵子树
		{"src-tauri/target", "src-tauri/target", true, "模式本身"},
		{"src-tauri/target", "src-tauri/target/debug/app", true, "前缀命中=整棵子树"},
		{"src-tauri/target", "apps/x/src-tauri/target", false, "锚定在根，不命中深层同名"},
		{"src-tauri/target", "src-tauri", false, "路径比模式短，父目录本身不该被排除"},
		{"src-tauri/target", "src-tauri/src/main.rs", false, "兄弟路径"},
		// 前导斜杠可省，语义相同
		{"/src-tauri/target", "src-tauri/target/debug", true, "前导 / 等价"},
		// 尾部斜杠不改变归类：仍是单段名字模式（任意深度）
		{"target/", "a/b/target/x", true, "尾部斜杠不触发锚定"},
		{"target/", "target", true, "尾部斜杠不触发锚定"},
		// ** 跨任意多段
		{"**/src-tauri/target", "apps/x/src-tauri/target/debug", true, "** 吞掉中间层"},
		{"**/src-tauri/target", "src-tauri/target", true, "** 可吞零段"},
		{"**/src-tauri/target", "apps/x/src-tauri/src", false, "** 后面仍需匹配"},
		// 段内通配
		{"packages/*/dist", "packages/ui/dist/index.js", true, "段内 *"},
		{"packages/*/dist", "packages/ui/src", false, "段内 * 不跨段"},
		{"packages/*/dist", "packages/a/b/dist", false, "* 只吃一段"},
		// 单段名字模式的历史行为必须不变
		{"node_modules", "a/node_modules/b", true, "单段任意深度"},
		{"*.log", "logs/app.log", true, "单段通配"},
		{"bin", "cabinet", false, "按段匹配，不是子串"},
	}
	for _, c := range cases {
		if got := IsIgnored(c.path, []string{c.pattern}); got != c.want {
			t.Errorf("IsIgnored(%q, [%q]) = %v, want %v（%s）", c.path, c.pattern, got, c.want, c.why)
		}
	}
}

func TestValidateIgnorePattern(t *testing.T) {
	for _, p := range []string{"target", "*.log", "src-tauri/target", "/a/b", "**/x", "packages/*/dist", "a/"} {
		if err := ValidateIgnorePattern(p); err != nil {
			t.Errorf("ValidateIgnorePattern(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{"[", "a/[", "/", "//"} {
		if err := ValidateIgnorePattern(p); err == nil {
			t.Errorf("ValidateIgnorePattern(%q) = nil, want an error", p)
		}
	}
}
