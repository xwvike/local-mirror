//go:build windows

package main

import "golang.org/x/sys/windows"

// enableConsoleUTF8 把控制台输出代码页切到 UTF-8（65001）。
// 程序所有输出（横幅、日志、--help）均为 UTF-8 编码的中文，而中文
// Windows 的控制台默认代码页是 GBK（936），不切换会整屏乱码。
// 返回恢复函数，正常退出路径还原用户会话原有代码页；进程没有附着
// 控制台（服务、重定向到文件的管道场景）时两个调用都失败，静默跳过
// ——重定向的字节流本就不经代码页解码，无需处理。
func enableConsoleUTF8() func() {
	old, err := windows.GetConsoleOutputCP()
	if err != nil {
		return func() {}
	}
	if old == 65001 || windows.SetConsoleOutputCP(65001) != nil {
		return func() {}
	}
	return func() { _ = windows.SetConsoleOutputCP(old) }
}
