//go:build windows

package main

// Windows 没有 SIGUSR1，热度快照暂无触发入口（观察需求以 macOS/Linux
// 服务端为主；将来如需要可挂到命名管道或本地状态接口上）
func installHeatDumpSignal() {}
