package main

import (
	"fmt"
	"local-mirror/internal/status"
	"local-mirror/pkg/termstyle"
	"path/filepath"
	"strings"
	"time"
)

// humanBytes 与 humanDuration 供 --status 展示
func humanStatusBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func humanSince(t time.Time) string {
	if t.IsZero() || t.Unix() == 0 {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm ago", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh ago", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func humanUptime(started int64) string {
	if started == 0 {
		return "?"
	}
	d := time.Since(time.Unix(started, 0))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func humanRate(bps float64) string {
	switch {
	case bps >= 1<<20:
		return fmt.Sprintf("%.1f MB/s", bps/(1<<20))
	case bps >= 1<<10:
		return fmt.Sprintf("%.0f KB/s", bps/(1<<10))
	case bps > 0:
		return fmt.Sprintf("%.0f B/s", bps)
	default:
		return "—"
	}
}

// progressBar 渲染定宽进度条 [████░░░░] 66%
func progressBar(done, total uint64, width int, p termstyle.Palette) string {
	if total == 0 {
		return p.Dim + strings.Repeat("░", width) + p.Reset + "   —"
	}
	frac := float64(done) / float64(total)
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * float64(width))
	return fmt.Sprintf("%s%s%s%s%s %3d%%",
		p.Green, strings.Repeat("█", filled), p.Dim, strings.Repeat("░", width-filled), p.Reset, int(frac*100))
}

func fileSuffix(name string, p termstyle.Palette) string {
	if name == "" {
		return ""
	}
	return fmt.Sprintf("  %s(%s)%s", p.Dim, name, p.Reset)
}

// padCell 按显示宽度右填充到 w 列（ANSI 色码会破坏 %-Ns 对齐，故先按纯文本
// 计宽再上色）
func padCell(text string, w int) string {
	if dw := termstyle.DisplayWidth(text); dw < w {
		return text + strings.Repeat(" ", w-dw)
	}
	return text
}

// memoryLine 组装内存展示：有 OS RSS 就以它为主，附 Go 堆/申请量
func memoryLine(s *status.Snapshot) string {
	heap := fmt.Sprintf("%s heap · %s sys", humanStatusBytes(s.HeapBytes), humanStatusBytes(s.SysBytes))
	if s.HasRSS {
		return fmt.Sprintf("%s rss   (%s)", humanStatusBytes(s.RSSBytes), heap)
	}
	return heap
}

func fdLine(s *status.Snapshot) string {
	if s.HasFDs {
		return fmt.Sprintf("%d", s.FDs)
	}
	return "— (not available on this platform)"
}

func dirShort(mode string) string {
	switch mode {
	case "reality":
		return "send"
	case "mirror":
		return "recv"
	case "relay":
		return "relay"
	}
	return mode
}

// dirShortFromSnap 从快照的方向字串（"send · source" 等）取短标签，
// 供 --all 使用（发现来的实例没有原始 mode，只有已渲染的方向串）
func dirShortFromSnap(s *status.Snapshot) string {
	switch {
	case strings.HasPrefix(s.Direction, "send"):
		return "send"
	case strings.HasPrefix(s.Direction, "receive"):
		return "recv"
	case strings.HasPrefix(s.Direction, "relay"):
		return "relay"
	}
	return s.Direction
}

// shortRoot 取同步根的末两段做行标签（如 proj/src），比单纯 basename 更能
// 区分"多个根同名 basename"（backup/src 与 proj/src）
func shortRoot(root string) string {
	base := filepath.Base(root)
	parent := filepath.Base(filepath.Dir(root))
	if parent == "." || parent == "/" || parent == "" {
		return base
	}
	return parent + "/" + base
}
