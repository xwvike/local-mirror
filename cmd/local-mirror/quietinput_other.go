//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package main

// quietInput 在非类 Unix 平台（如 Windows）暂为空操作：闪烁修复（同步刷新）
// 与这里无关、跨平台通用；关回显留待各平台的控制台 API 后续单独接。
func quietInput() func() { return func() {} }
