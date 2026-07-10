// Package tui 提供启动期使用的最小终端交互组件。
// 手写 raw-mode 实现（仅依赖 golang.org/x/term），与项目手写 ANSI 的
// banner 风格一致，不引入 TUI 框架
package tui

import (
	"fmt"
	"local-mirror/pkg/termstyle"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"
)

// Option 选择列表中的一项
type Option struct {
	Alias string
	Addr  string
	Path  string
}

// Outcome 选择器的退出方式
type Outcome int

const (
	Selected Outcome = iota // 用户回车选中 idx 项
	Canceled                // q / Ctrl-C / ESC 主动取消
	Rescan                  // r 请求重新扫描
)

const (
	aliasColMax = 20 // 别名列显示宽度上限
	keyCtrlC    = 0x03
	keyEnter    = '\r'
	keyEsc      = 0x1b
)

// Select 在终端上渲染一个方向键选择列表并阻塞等待用户操作。
// 调用方必须已确认 stdin 与 stdout 都是终端。
// opts 为空时渲染空态（未发现服务端），只接受 r（重扫）与 q（退出）
func Select(title string, opts []Option) (int, Outcome, error) {
	stdinFd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return 0, Canceled, fmt.Errorf("进入终端 raw 模式失败: %w", err)
	}
	// 恢复终端的 defer 必须先于一切输出注册：Select 内部 panic 时
	// 终端也绝不能停留在 raw 模式（重复 Restore 无害）
	restore := func() {
		_ = term.Restore(stdinFd, oldState)
		fmt.Print("\x1b[?25h") // 恢复光标
	}
	defer restore()

	// raw 模式下 Ctrl-C 不再产生 SIGINT（按字节处理），但外部 SIGTERM
	// 仍可能到达；兜底恢复终端再退出
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		if _, ok := <-sigCh; ok {
			restore()
			os.Exit(1)
		}
	}()

	fmt.Print("\x1b[?25l") // 隐藏光标

	p := termstyle.NewPalette(os.Stdout)
	cursor := 0
	lines := 0 // 上一帧渲染的行数，重绘时上移覆盖

	render := func() {
		if lines > 0 {
			fmt.Printf("\x1b[%dA\r", lines)
		}
		lines = renderFrame(p, title, opts, cursor)
	}
	render()

	buf := make([]byte, 3)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return 0, Canceled, fmt.Errorf("读取按键失败: %w", err)
		}
		if n == 0 {
			continue
		}
		key := buf[0]
		switch {
		case key == keyEsc && n >= 3 && buf[1] == '[': // 方向键序列
			switch buf[2] {
			case 'A':
				if len(opts) > 0 && cursor > 0 {
					cursor--
					render()
				}
			case 'B':
				if len(opts) > 0 && cursor < len(opts)-1 {
					cursor++
					render()
				}
			}
		case key == 'k':
			if len(opts) > 0 && cursor > 0 {
				cursor--
				render()
			}
		case key == 'j':
			if len(opts) > 0 && cursor < len(opts)-1 {
				cursor++
				render()
			}
		case key == keyEnter:
			if len(opts) == 0 {
				continue
			}
			fmt.Printf("%s已选择:%s %s (%s)\r\n", p.Green, p.Reset, opts[cursor].Alias, opts[cursor].Addr)
			return cursor, Selected, nil
		case key == 'r':
			fmt.Printf("%s重新扫描…%s\r\n", p.Dim, p.Reset)
			return 0, Rescan, nil
		case key == 'q', key == keyCtrlC, key == keyEsc: // 裸 ESC（n==1）走到这里
			fmt.Printf("%s已取消%s\r\n", p.Dim, p.Reset)
			return 0, Canceled, nil
		}
	}
}

// renderFrame 画一帧，返回渲染的行数。
// 每行先 \x1b[K 清行再输出，raw 模式下显式 \r\n 换行
func renderFrame(p termstyle.Palette, title string, opts []Option, cursor int) int {
	termWidth := 80
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		termWidth = w
	}

	var b strings.Builder
	line := func(s string) {
		b.WriteString("\x1b[K")
		b.WriteString(s)
		b.WriteString("\r\n")
	}

	line(fmt.Sprintf("%s%s%s", p.Bold, title, p.Reset))

	if len(opts) == 0 {
		line(fmt.Sprintf("  %s未发现服务端%s", p.Dim, p.Reset))
		line(fmt.Sprintf("  %sr 重新扫描 · q 退出%s", p.Dim, p.Reset))
		fmt.Print(b.String())
		return 3
	}

	aliasCol := 0
	addrCol := 0
	for _, o := range opts {
		aliasCol = max(aliasCol, termstyle.DisplayWidth(o.Alias))
		addrCol = max(addrCol, termstyle.DisplayWidth(o.Addr))
	}
	aliasCol = min(aliasCol, aliasColMax)

	for i, o := range opts {
		alias := termstyle.Truncate(o.Alias, aliasCol)
		aliasPad := strings.Repeat(" ", max(0, aliasCol-termstyle.DisplayWidth(alias)))
		addrPad := strings.Repeat(" ", max(0, addrCol-termstyle.DisplayWidth(o.Addr)))
		// 前缀 4 列（"  ❯ "）+ 两列间距×2
		pathWidth := termWidth - 4 - aliasCol - 2 - addrCol - 2 - 1
		path := termstyle.Truncate(o.Path, max(8, pathWidth))

		if i == cursor {
			line(fmt.Sprintf("  %s%s❯ %s%s%s  %s%s%s%s  %s%s%s",
				p.Bold, p.Cyan, alias, p.Reset, aliasPad,
				p.Green, o.Addr, p.Reset, addrPad,
				p.Dim, path, p.Reset))
		} else {
			line(fmt.Sprintf("    %s%s  %s%s%s%s  %s%s%s",
				alias, aliasPad,
				p.Green, o.Addr, p.Reset, addrPad,
				p.Dim, path, p.Reset))
		}
	}
	line(fmt.Sprintf("  %s↑↓/jk 移动 · Enter 选择 · r 重新扫描 · q 退出%s", p.Dim, p.Reset))
	fmt.Print(b.String())
	return len(opts) + 2
}
