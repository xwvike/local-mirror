//go:build !windows

package main

// enableConsoleUTF8 仅 Windows 需要（控制台代码页），其它平台为空操作
func enableConsoleUTF8() func() { return func() {} }
