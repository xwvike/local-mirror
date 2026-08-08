package main

import (
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/keyfile"
	"local-mirror/internal/logger"
	"local-mirror/internal/network"
	"local-mirror/pkg/termstyle"
	"net"
	"os"
	"strconv"
	"strings"
)

// directionLabel/transportLabel/peerLabel 供 status 与人读展示：把内部 mode +
// 四象限状态翻译成方向优先的词汇
func directionLabel() string {
	switch *config.Mode {
	case "reality":
		return "send · source"
	case "mirror":
		return "receive · sink"
	case "relay":
		return "relay"
	}
	return *config.Mode
}

func transportLabel() string {
	if config.TransportListens() {
		return "listen"
	}
	return "dial"
}

func peerLabel() string {
	if config.TransportListens() {
		return "inbound"
	}
	if config.DiscoveredAddr != "" {
		return config.DiscoveredAddr
	}
	host, port := network.SplitPeer(*config.RealityIP)
	if host == "" {
		return "(LAN discovery)"
	}
	if port == 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// printBanner 向 stdout 输出启动状态。
// 长驻进程默认日志级别下终端不应完全静默，用户需要知道进程在做什么
// bannerFont 字标用的 3×5 像素点阵（M 为 5 像素宽、连字符 2 像素），
// 每字符一列串代表一行像素。渲染时两行像素折叠进一个字符格
// （▀ 上、▄ 下、█ 满），因此 5 行像素只占 3 行终端
var bannerFont = map[rune][]string{
	'L': {"100", "100", "100", "100", "111"},
	'O': {"111", "101", "101", "101", "111"},
	'C': {"111", "100", "100", "100", "111"},
	'A': {"111", "101", "111", "101", "101"},
	'M': {"10001", "11011", "10101", "10001", "10001"},
	'I': {"111", "010", "010", "010", "111"},
	'R': {"111", "101", "110", "101", "101"},
	'-': {"00", "00", "11", "00", "00"},
}

// renderWordmark 把单词渲染为 3 行半块字符画。字形间距 1 像素；
// "LOCAL-MIRROR" 全串恰好 48 像素宽，与横幅同宽
func renderWordmark(word string) []string {
	bitmap := make([]string, 5)
	for i, ch := range word {
		g, ok := bannerFont[ch]
		if !ok {
			continue
		}
		for r := range bitmap {
			// 连字符收紧左侧字距（side bearing）：字母统一 1 像素间距，
			// 但 '-' 笔画少且悬在中排，两侧都空一列会把词拆散；贴住左邻
			//（其右列中排本为空，不会粘笔画）、右侧保留 1 列呼吸
			if i > 0 && ch != '-' {
				bitmap[r] += "0"
			}
			bitmap[r] += g[r]
		}
	}
	bitmap = append(bitmap, strings.Repeat("0", len(bitmap[0]))) // 补齐偶数像素行
	out := make([]string, 0, 3)
	for r := 0; r < 6; r += 2 {
		var b strings.Builder
		for c := 0; c < len(bitmap[r]); c++ {
			up, down := bitmap[r][c] == '1', bitmap[r+1][c] == '1'
			switch {
			case up && down:
				b.WriteRune('█')
			case up:
				b.WriteRune('▀')
			case down:
				b.WriteRune('▄')
			default:
				b.WriteRune(' ')
			}
		}
		out = append(out, b.String())
	}
	return out
}

func printBanner() {
	p := termstyle.NewPalette(os.Stdout)
	const width = 48
	const labelWidth = 11

	line := p.Dim + strings.Repeat("─", width) + p.Reset
	row := func(label, value string) {
		pad := strings.Repeat(" ", max(1, labelWidth-termstyle.DisplayWidth(label)))
		fmt.Printf("  %s%s%s%s%s\n", p.Dim, label, p.Reset, pad, value)
	}

	// 方向优先：横幅用 send/receive 说话，-m 老词汇只是别名
	modeDescMap := map[string]string{"reality": "send · source", "mirror": "receive · sink", "relay": "relay"}
	modeDesc := modeDescMap[*config.Mode]

	// 字标横幅：单行 "LOCAL-MIRROR"，实与虚用亮度表达——LOCAL 亮青、
	// MIRROR 压暗（这个字号下用 ░ 会糊，亮度对比才能保住字形）。
	// 前段 "LOCAL-" 与后段 "MIRROR" 分别渲染后逐行拼接，中间补一个
	// 字形间距像素列
	fmt.Println()
	solid := renderWordmark("LOCAL-")
	ghost := renderWordmark("MIRROR")
	for r := range solid {
		fmt.Printf("%s%s%s %s%s%s%s\n",
			p.Cyan, solid[r], p.Reset, p.Cyan, p.Dim, ghost[r], p.Reset)
	}
	fmt.Println()

	fmt.Println(line)
	fmt.Printf("  %s%sLocal Mirror%s %s  ·  %s%s%s (%s)\n",
		p.Bold, p.Cyan, p.Reset, version, p.Bold, *config.Mode, p.Reset, modeDesc)
	fmt.Println(line)
	row("Sync root", config.StartPath)
	// 忽略规则最多展示 4 条，其余折叠为计数（完整列表见 --help 与配置）
	ignoreShown := config.IgnoreFileList
	suffix := ""
	if len(ignoreShown) > 4 {
		suffix = fmt.Sprintf(" %s(+%d)%s", p.Dim, len(ignoreShown)-4, p.Reset)
		ignoreShown = ignoreShown[:4]
	}
	row("Ignores", strings.Join(ignoreShown, ", ")+suffix)
	if config.SyncsFromUpstream() && !config.SinkListens {
		switch {
		case config.DiscoveredAddr != "":
			row("Upstream", fmt.Sprintf("%s%s%s %s(discovered: %s)%s",
				p.Green, config.DiscoveredAddr, p.Reset, p.Dim, config.DiscoveredAlias, p.Reset))
		default:
			host, port := network.SplitPeer(*config.RealityIP)
			if host == "" {
				host = "127.0.0.1"
			}
			if port != 0 {
				row("Upstream", fmt.Sprintf("%s%s%s %s(pinned port)%s",
					p.Green, net.JoinHostPort(host, strconv.Itoa(port)), p.Reset, p.Dim, p.Reset))
			} else {
				row("Upstream", fmt.Sprintf("%s%s%s %s(port scan %d-%d)%s",
					p.Green, host, p.Reset, p.Dim, config.DefaultPort, config.DefaultPort+config.PortScanRange-1, p.Reset))
			}
		}
	}
	// 汇监听格：上游没有地址可显，等源拨入
	if config.SinkListens {
		row("Source", fmt.Sprintf("inbound %s(waiting for the source to dial us)%s", p.Dim, p.Reset))
	}
	// 源拨出格：对端是监听中的汇
	if config.SourceDials {
		host, port := network.SplitPeer(*config.RealityIP)
		if port == 0 {
			port = config.DefaultPort
		}
		row("Sink", fmt.Sprintf("%s%s%s %s(dialing out; the sink listens)%s",
			p.Green, net.JoinHostPort(host, strconv.Itoa(port)), p.Reset, p.Dim, p.Reset))
	}
	// 监听行属于任何监听的一方：经典源、relay 下游、以及汇监听格
	if config.TransportListens() {
		if network.ListenedDualStack {
			row("Listen", fmt.Sprintf("%s:%d%s %s(IPv4 + IPv6)%s", p.Green, config.ActualPort, p.Reset, p.Dim, p.Reset))
		} else {
			row("Listen", fmt.Sprintf("%s0.0.0.0:%d%s %s(IPv4 only; host has no IPv6)%s", p.Green, config.ActualPort, p.Reset, p.Dim, p.Reset))
		}
	}
	switch {
	case *config.Secret != "" && config.SecretFromKeyFile:
		row("Encryption", fmt.Sprintf("%son%s (Noise NNpsk0, key file, fp %s)", p.Green, p.Reset, keyfile.Fingerprint(*config.Secret)))
	case *config.Secret != "":
		row("Encryption", fmt.Sprintf("%son%s (Noise NNpsk0, fp %s)", p.Green, p.Reset, keyfile.Fingerprint(*config.Secret)))
	case *config.NoEncrypt:
		row("Encryption", fmt.Sprintf("off %s(--no-encrypt: forced plaintext)%s", p.Dim, p.Reset))
	default:
		row("Encryption", fmt.Sprintf("off %s(plaintext; enable with -k or --gen-key)%s", p.Dim, p.Reset))
	}
	// 仅同步方（mirror/relay）涉及删除，展示当前删除策略
	if config.SyncsFromUpstream() {
		if *config.AllowDelete {
			row("Deletion", fmt.Sprintf("%son%s %s(faithful mirror; local extras get deleted)%s", p.Green, p.Reset, p.Dim, p.Reset))
		} else {
			row("Deletion", fmt.Sprintf("off %s(additive only; local extras kept)%s", p.Dim, p.Reset))
		}
		// 关键路径解锁档：提示覆盖前会快照备份
		if config.SnapshotOverwrites {
			row("Critical", fmt.Sprintf("%sunlocked%s %s(--allow-critical; first overwrite backed up to .local-mirror/backups)%s",
				p.Green, p.Reset, p.Dim, p.Reset))
		}
	}
	row("Instance", fmt.Sprintf("%08x", config.InstanceID))
	row("PID", fmt.Sprintf("%d", os.Getpid()))
	row("Log", fmt.Sprintf("%s %s(level %s)%s", logger.LogPath(), p.Dim, *config.LogLevel, p.Reset))
	fmt.Println(line)
	// 提示观测命令：--status 是独立只读进程，不惊动本服务
	fmt.Printf("  %swatch:  local-mirror --status -p %s   %s(or --status --all)%s\n",
		p.Dim, config.StartPath, p.Dim, p.Reset)
	fmt.Println()
}
