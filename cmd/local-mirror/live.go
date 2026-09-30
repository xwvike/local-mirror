package main

import (
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/logger"
	"local-mirror/internal/status"
	"local-mirror/pkg/termstyle"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// liveStatus 前台交互运行（标准输出为终端）时，在横幅下方每秒原地重绘连接与传输状态。
// 面板显示期间日志只写文件，最近几条显示在面板里；作为服务运行、输出被重定向或由
// --config 监管的子进程（输出是管道）都不启用
type liveStatus struct {
	lines int
	stop  chan struct{}
	done  chan struct{}
}

// startLiveStatus 满足条件时开始刷新，否则返回 nil（Stop 对 nil 是空操作）
func startLiveStatus() *liveStatus {
	if !term.IsTerminal(int(os.Stdout.Fd())) || !enableVirtualTerminal() || !logger.DetachTerminal() {
		return nil
	}
	l := &liveStatus{stop: make(chan struct{}), done: make(chan struct{})}
	fmt.Print("\033[?25l") // 隐藏光标
	go l.loop()
	return l
}

func (l *liveStatus) loop() {
	defer close(l.done)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		l.draw()
		select {
		case <-l.stop:
			return
		case <-t.C:
		}
	}
}

// Stop 停止刷新：画最后一帧并留在终端上，恢复光标与日志的终端输出
func (l *liveStatus) Stop() {
	if l == nil {
		return
	}
	close(l.stop)
	<-l.done
	l.draw()
	fmt.Print("\033[?25h")
	logger.AttachTerminal()
}

// draw 回到上一帧的起点、清到屏幕末尾再整帧重画。每行按终端宽度截断，
// 不会折行，上移的行数因此总与上一帧一致
func (l *liveStatus) draw() {
	width := 80
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		width = w
	}
	frame := liveFrame(status.Live(), logger.Recent(), termstyle.NewPalette(os.Stdout), width)
	var b strings.Builder
	b.WriteString("\033[?2026h") // 同步刷新：支持的终端整帧原子呈现，不闪烁
	if l.lines > 0 {
		fmt.Fprintf(&b, "\033[%dA\r\033[J", l.lines)
	}
	for _, line := range frame {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteString("\033[?2026l")
	os.Stdout.WriteString(b.String())
	l.lines = len(frame)
}

func liveFrame(s status.Snapshot, recent []string, p termstyle.Palette, width int) []string {
	const labelWidth = 11
	var out []string
	add := func(line string) { out = append(out, fitLine(line, width, p)) }
	row := func(label, value string) {
		pad := strings.Repeat(" ", max(1, labelWidth-termstyle.DisplayWidth(label)))
		add(fmt.Sprintf("  %s%s%s%s%s", p.Dim, label, p.Reset, pad, value))
	}

	if s.Connected {
		link := s.Detail
		if s.Peers > 1 {
			link += fmt.Sprintf(" %s(%d connections)%s", p.Dim, s.Peers, p.Reset)
		}
		row("Link", fmt.Sprintf("%s●%s %s", p.Green, p.Reset, link))
	} else {
		row("Link", fmt.Sprintf("%s○ %s%s", p.Dim, waitingText(s), p.Reset))
	}

	if s.CurrentFile != "" {
		row("Transfer", fmt.Sprintf("%s▶%s %s", p.Cyan, p.Reset, s.CurrentFile))
		row("", fmt.Sprintf("%s  %s / %s  %s%s%s",
			progressBar(s.CurrentDone, s.CurrentTotal, 20, p),
			humanStatusBytes(s.CurrentDone), humanStatusBytes(s.CurrentTotal),
			p.Bold, humanRate(s.RateBps), p.Reset))
	} else {
		state := "idle"
		if s.RateBps > 0 {
			state = humanRate(s.RateBps)
		}
		row("Transfer", fmt.Sprintf("%s%s%s", p.Dim, state, p.Reset))
	}
	row("Session", fmt.Sprintf("%s / %d files   %s· last %s%s%s",
		humanStatusBytes(s.Bytes), s.Files, p.Dim, humanSince(time.Unix(s.LastSyncUnix, 0)), fileSuffix(s.LastFile, p), p.Reset))
	if s.Errors > 0 {
		row("Errors", fmt.Sprintf("%s%d%s", p.Yellow, s.Errors, p.Reset))
	} else {
		row("Errors", "0")
	}
	for i, r := range recent {
		label := ""
		if i == 0 {
			label = "Recent"
		}
		row(label, p.Dim+r+p.Reset)
	}
	add(p.Dim + strings.Repeat("─", 48) + p.Reset)
	add(fmt.Sprintf("  %sCtrl-C to stop · up %s%s", p.Dim, humanUptime(s.StartedUnix), p.Reset))
	return out
}

// waitingText 未连接时按本端的传输方向说明在等什么
func waitingText(s status.Snapshot) string {
	switch {
	case s.Detail != "":
		return s.Detail
	case config.SinkListens:
		return "waiting for the source to dial in"
	case config.TransportListens():
		return "waiting for a receiver to connect"
	default:
		return "connecting to " + s.Peer
	}
}

// fitLine 把含颜色控制序列的一行截断到终端宽度以内（控制序列不占列宽），
// 超出时以 "…" 结尾。留出最后一列，避免终端在行尾自动折行
func fitLine(s string, cols int, p termstyle.Palette) string {
	limit := cols - 1
	if visibleWidth(s) <= limit {
		return s
	}
	var b strings.Builder
	w := 0
	for i := 0; i < len(s); {
		if n := escapeLen(s[i:]); n > 0 {
			b.WriteString(s[i : i+n])
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if w+runeWidth(r) > limit-1 {
			break
		}
		b.WriteRune(r)
		w += runeWidth(r)
		i += size
	}
	return b.String() + "…" + p.Reset
}

// escapeLen 返回 s 开头 CSI 控制序列（ESC [ … 终止字节）的长度，不是则为 0
func escapeLen(s string) int {
	if len(s) < 2 || s[0] != 0x1b || s[1] != '[' {
		return 0
	}
	j := 2
	for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
		j++
	}
	return min(j+1, len(s))
}

func runeWidth(r rune) int {
	if r >= 0x2E80 {
		return 2
	}
	return 1
}

func visibleWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if n := escapeLen(s[i:]); n > 0 {
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w += runeWidth(r)
		i += size
	}
	return w
}
