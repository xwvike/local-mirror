package termstyle

import "testing"

func TestDisplayWidth(t *testing.T) {
	cases := map[string]int{
		"":            0,
		"abc":         3,
		"图片库":         6,
		"mac-图片":      8,
		"10.0.0.1:52": 11,
	}
	for s, want := range cases {
		if got := DisplayWidth(s); got != want {
			t.Errorf("DisplayWidth(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		s       string
		maxCols int
		want    string
	}{
		{"abc", 10, "abc"},     // 不超限原样返回
		{"abcdef", 4, "abc…"},  // ASCII 截断
		{"图片库目录", 10, "图片库目录"}, // CJK 恰好等宽
		{"图片库目录", 6, "图片…"},    // CJK 截断（2 列字符不切半）
		{"图片库目录", 7, "图片库…"},   // 奇数列，CJK 放不下时留给省略号
		{"abcdefgh", 1, "…"},   // 极限窄
		{"图aa图bb", 5, "图aa…"},  // 混合宽度，恰好填满 5 列
	}
	for _, c := range cases {
		if got := Truncate(c.s, c.maxCols); got != c.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", c.s, c.maxCols, got, c.want)
		}
	}
	// 截断结果的显示宽度绝不超过 maxCols
	for _, s := range []string{"图片库目录很长的路径", "abcdefghijk", "图a图a图a图a"} {
		for cols := 1; cols < 15; cols++ {
			if w := DisplayWidth(Truncate(s, cols)); w > cols {
				t.Errorf("Truncate(%q, %d) width %d exceeds limit", s, cols, w)
			}
		}
	}
}
