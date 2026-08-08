package main

import (
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/status"
	"local-mirror/pkg/termstyle"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// observeWarm 一次性观测（管道/重定向）投放心跳后，给常驻进程被 fsnotify 唤醒
// 并落一版盘的时间——常驻进程默认不写，靠观测心跳触发（用户不看就不写）
const observeWarm = 400 * time.Millisecond

// runStatusSingle 单实例 --status：终端进实时刷新循环，管道则打印一次。
// 观测进程投放心跳请求常驻进程落盘，读完撤销——无人看时常驻进程完全不写
func runStatusSingle(root string) {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		status.TouchObserve(root)
		liveLoop(func() { status.TouchObserve(root); renderSingle(root) })
		status.ClearObserve(root)
	} else {
		since := time.Now()
		status.TouchObserve(root)
		status.AwaitFresh(root, since, 2*time.Second)
		renderSingle(root)
		status.ClearObserve(root)
	}
}

// runStatusAggregate 多实例 --status --config：聚合 YAML 每个任务的状态
func runStatusAggregate(cfg *config.MultiConfig) {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		liveLoop(func() { renderAggregate(cfg) })
	} else {
		for i := range cfg.Tasks {
			status.TouchObserve(cfg.Tasks[i].Path)
		}
		time.Sleep(observeWarm)
		renderAggregate(cfg)
	}
}

// liveLoop 实时刷新：备用屏 + 隐藏光标，每秒重绘一帧，Ctrl-C 退出并还原终端。
// 走 os.Exit 会跳过 defer，故进出终端态都显式做，不依赖 defer
func liveLoop(frame func()) {
	p := termstyle.NewPalette(os.Stdout)
	// 刷新期间关掉输入回显：否则键入/滚轮产生的转义序列会被回显到备用屏上，
	// 显得很脏。保留 ISIG（Ctrl-C 仍生成 SIGINT，沿用下面的信号退出）与输出
	// 处理（\n→\r\n，否则整屏阶梯错位）。非 TTY 或失败时是空操作。
	restoreInput := quietInput()
	fmt.Print("\033[?1049h\033[?25l") // 备用屏 + 隐藏光标
	leave := func() {
		fmt.Print("\033[?25h\033[?1049l")
		restoreInput()
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	t := time.NewTicker(1 * time.Second)
	for {
		// 同步刷新（DEC 私有模式 2026）：把「清屏 + 重绘」包成一次原子提交，
		// 支持的终端（iTerm2/kitty/wezterm/ghostty/alacritty/tmux≥3.4）不再露出
		// 清屏后重绘前的空屏 → 消除闪烁；不支持的终端会忽略该序列，退化为原行为。
		fmt.Print("\033[?2026h\033[H\033[2J") // 开始同步 + 光标归位 + 清屏
		frame()
		fmt.Printf("\n  %srefresh 1s · Ctrl-C to exit%s\n", p.Dim, p.Reset)
		fmt.Print("\033[?2026l") // 结束同步（原子呈现整帧）
		select {
		case <-sig:
			t.Stop()
			leave()
			return
		case <-t.C:
		}
	}
}

// renderSingle 渲染单个实例的运行时快照（每帧重新读盘）。
// 无快照文件 = 没有实例在此根跑过；快照陈旧 = 进程可能已停
func renderSingle(root string) {
	p := termstyle.NewPalette(os.Stdout)
	snap, err := status.Load(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: cannot read status: %v\n", err)
		return
	}
	if snap == nil {
		fmt.Printf("\n  %sno status for%s %s\n", p.Dim, p.Reset, root)
		fmt.Printf("  %s(no instance has run here, or .local-mirror/status.json was removed)%s\n", p.Dim, p.Reset)
		return
	}

	const width = 54
	const labelWidth = 12
	line := p.Dim + strings.Repeat("─", width) + p.Reset
	row := func(label, value string) {
		pad := strings.Repeat(" ", max(1, labelWidth-termstyle.DisplayWidth(label)))
		fmt.Printf("  %s%s%s%s%s\n", p.Dim, label, p.Reset, pad, value)
	}

	// 存活判据：陈旧快照说明落盘循环停了（进程多半已退）——替代 ps 的那一眼
	live := !snap.Stale()
	fmt.Println()
	fmt.Println(line)
	if live {
		fmt.Printf("  %s%sStatus%s      %s%s● running%s   %spid %d · up %s%s\n",
			p.Bold, p.Cyan, p.Reset, p.Bold, p.Green, p.Reset, p.Dim, snap.PID, humanUptime(snap.StartedUnix), p.Reset)
	} else {
		fmt.Printf("  %s%sStatus%s      %s○ stale%s   %slast update %s · pid %d may be dead%s\n",
			p.Bold, p.Cyan, p.Reset, p.Yellow, p.Reset, p.Dim, humanSince(time.Unix(snap.UpdatedUnix, 0)), snap.PID, p.Reset)
	}
	fmt.Println(line)

	row("Direction", fmt.Sprintf("%s   %s(%s)%s", snap.Direction, p.Dim, snap.Transport, p.Reset))
	row("Peer", snap.Peer)
	switch {
	case !live && snap.Connected:
		// 进程已停：连接字段是死前的最后已知态，别用绿色误导成"此刻在连"
		row("Link", fmt.Sprintf("%s○ %s (last known)%s", p.Dim, snap.Detail, p.Reset))
	case snap.Connected:
		row("Link", fmt.Sprintf("%s● %s%s", p.Green, snap.Detail, p.Reset))
	default:
		detail := "idle (no active connection)"
		if snap.Detail != "" {
			detail = snap.Detail
		}
		row("Link", fmt.Sprintf("%s○ %s%s", p.Dim, detail, p.Reset))
	}
	enc := "off (plaintext)"
	if snap.Encrypted {
		enc = "on (Noise NNpsk0)"
	}
	row("Encryption", enc)
	row("Sync root", snap.Root)
	fmt.Println(line)

	// 传输段：进行中的文件带进度条 + 速率；空闲则只显速率/idle
	if live && snap.CurrentFile != "" {
		row("Transfer", fmt.Sprintf("%s▶%s %s", p.Cyan, p.Reset, snap.CurrentFile))
		row("", fmt.Sprintf("%s   %s / %s   %s%s%s",
			progressBar(snap.CurrentDone, snap.CurrentTotal, 20, p),
			humanStatusBytes(snap.CurrentDone), humanStatusBytes(snap.CurrentTotal),
			p.Bold, humanRate(snap.RateBps), p.Reset))
	} else {
		state := "idle"
		if live && snap.RateBps > 0 {
			state = humanRate(snap.RateBps)
		}
		row("Transfer", fmt.Sprintf("%s%s%s", p.Dim, state, p.Reset))
	}
	row("Totals", fmt.Sprintf("%s / %d files   %s· last %s%s%s",
		humanStatusBytes(snap.Bytes), snap.Files, p.Dim, humanSince(time.Unix(snap.LastSyncUnix, 0)), fileSuffix(snap.LastFile, p), p.Reset))
	if snap.Errors > 0 {
		row("Errors", fmt.Sprintf("%s%d%s", p.Yellow, snap.Errors, p.Reset))
	} else {
		row("Errors", "0")
	}
	fmt.Println(line)

	// 资源段（常驻进程自采）
	row("CPU", fmt.Sprintf("%.1f%%", snap.CPUPercent))
	row("Memory", memoryLine(snap))
	row("FDs", fdLine(snap))
	row("Goroutines", fmt.Sprintf("%d", snap.Goroutines))
	fmt.Println(line)
}

// statusRow 聚合表的一行。Snap 为 nil 表示该行对应的实例未启动
type statusRow struct {
	Name string
	Dir  string
	Snap *status.Snapshot
}

// renderStatusTable 渲染聚合表：每实例一行，列对齐（色码不计入列宽，见 padCell）。
// --config（YAML 多任务）与 --all（进程表发现）共用
func renderStatusTable(rows []statusRow, p termstyle.Palette) {
	fmt.Printf("  %s%s %s %s %s %s %s %s %s%s\n", p.Dim,
		padCell("NAME", 16), padCell("DIR", 6), padCell("LINK", 5),
		padCell("RATE", 11), padCell("FILES", 7), padCell("LAST", 9),
		padCell("CPU", 6), padCell("MEM", 10), p.Reset)

	for _, r := range rows {
		snap := r.Snap
		rate, files, last, cpu, mem := "—", "—", "—", "—", "—"
		var link string
		switch {
		case snap == nil:
			link = p.Dim + padCell("—", 5) + p.Reset
		case snap.Stale():
			link = p.Yellow + padCell("○", 5) + p.Reset
		case snap.Connected:
			link = p.Green + padCell("●", 5) + p.Reset
		default:
			link = p.Dim + padCell("○", 5) + p.Reset
		}
		if snap != nil {
			if snap.RateBps > 0 {
				rate = humanRate(snap.RateBps)
			}
			files = fmt.Sprintf("%d", snap.Files)
			last = humanSince(time.Unix(snap.LastSyncUnix, 0))
			cpu = fmt.Sprintf("%.1f%%", snap.CPUPercent)
			if snap.HasRSS {
				mem = humanStatusBytes(snap.RSSBytes)
			} else {
				mem = humanStatusBytes(snap.HeapBytes)
			}
		}
		suffix := ""
		if snap != nil && snap.Stale() {
			suffix = p.Yellow + "  (stale)" + p.Reset
		} else if snap == nil {
			suffix = p.Dim + "  (not started)" + p.Reset
		}
		fmt.Printf("  %s %s %s %s %s %s %s %s%s\n",
			padCell(termstyle.Truncate(r.Name, 16), 16), padCell(r.Dir, 6), link,
			padCell(rate, 11), padCell(files, 7), padCell(last, 9),
			padCell(cpu, 6), padCell(mem, 10), suffix)
	}
}

// renderAggregate 渲染 YAML 多任务的聚合表：每任务一行，各读各自根下的快照
func renderAggregate(cfg *config.MultiConfig) {
	p := termstyle.NewPalette(os.Stdout)
	fmt.Println()
	fmt.Printf("  %s%slocal-mirror%s   %s%d tasks%s\n", p.Bold, p.Cyan, p.Reset, p.Dim, len(cfg.Tasks), p.Reset)
	fmt.Println()
	rows := make([]statusRow, 0, len(cfg.Tasks))
	for i := range cfg.Tasks {
		t := cfg.Tasks[i]
		status.TouchObserve(t.Path) // 请求各任务下一帧刷新
		snap, _ := status.Load(t.Path)
		rows = append(rows, statusRow{Name: t.Name, Dir: dirShort(t.Mode), Snap: snap})
	}
	renderStatusTable(rows, p)
}

// runStatusAll 全机发现视图：终端进实时刷新循环，管道则打印一次
func runStatusAll() {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		liveLoop(renderAll)
	} else {
		for _, inst := range status.DiscoverInstances() {
			status.TouchObserve(inst.Root)
		}
		time.Sleep(observeWarm)
		renderAll()
	}
}

// renderAll 从进程表发现本机所有运行中的实例并聚合展示（每帧重新发现）
func renderAll() {
	p := termstyle.NewPalette(os.Stdout)
	instances := status.DiscoverInstances()
	fmt.Println()
	fmt.Printf("  %s%slocal-mirror%s   %s%d running on this host%s\n",
		p.Bold, p.Cyan, p.Reset, p.Dim, len(instances), p.Reset)
	if len(instances) == 0 {
		fmt.Printf("\n  %sno running local-mirror instances found%s\n", p.Dim, p.Reset)
		fmt.Printf("  %s(--all scans the process table for daemons that write .local-mirror/status.json;\n", p.Dim)
		fmt.Printf("   pre-status builds won't appear)%s\n", p.Reset)
		return
	}
	fmt.Println()
	rows := make([]statusRow, 0, len(instances))
	for _, inst := range instances {
		status.TouchObserve(inst.Root) // 请求各实例下一帧刷新
		rows = append(rows, statusRow{Name: shortRoot(inst.Root), Dir: dirShortFromSnap(inst.Snap), Snap: inst.Snap})
	}
	renderStatusTable(rows, p)
}
