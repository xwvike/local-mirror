// Package termstyle 提供终端 ANSI 样式与显示宽度计算的最小工具集，
// 由启动横幅与 TUI 选择器共用。
package termstyle

import (
	"os"
	"unicode/utf8"
)

// Palette ANSI 颜色码集合。零值即"无色"，所有字段为空串，可直接拼接
type Palette struct {
	Bold, Dim, Cyan, Green, Yellow, Reset string
}

// NewPalette 根据目标文件是否为终端决定是否启用颜色。
// 遵守 NO_COLOR 约定 (https://no-color.org)；管道/重定向时输出纯文本
func NewPalette(f *os.File) Palette {
	fi, err := f.Stat()
	isTTY := err == nil && fi.Mode()&os.ModeCharDevice != 0
	if !isTTY || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return Palette{}
	}
	return Palette{
		Bold:   "\033[1m",
		Dim:    "\033[2m",
		Cyan:   "\033[36m",
		Green:  "\033[32m",
		Yellow: "\033[33m",
		Reset:  "\033[0m",
	}
}

// DisplayWidth 计算终端显示宽度：CJK 字符占两列，ASCII 占一列
func DisplayWidth(s string) int {
	w := 0
	for _, r := range s {
		if r >= 0x2E80 {
			w += 2
		} else {
			w++
		}
	}
	return w
}

// Truncate 把 s 截断到不超过 maxCols 显示列，超出时以"…"结尾。
// 按 rune 迭代，不会切坏多字节字符
func Truncate(s string, maxCols int) string {
	if DisplayWidth(s) <= maxCols {
		return s
	}
	if maxCols <= 1 {
		return "…"
	}
	w := 0
	out := make([]rune, 0, utf8.RuneCountInString(s))
	for _, r := range s {
		rw := 1
		if r >= 0x2E80 {
			rw = 2
		}
		// 预留 1 列给省略号
		if w+rw > maxCols-1 {
			break
		}
		w += rw
		out = append(out, r)
	}
	return string(out) + "…"
}
