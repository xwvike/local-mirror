package main

import "io/fs"

// fileOwnerUID Windows 没有 POSIX 属主概念，永远报告"取不到"。
// 调用方（chownConfigTo）因此会跳过"属主已正确"的快路径；
// 而 Windows 分支在 serviceInstall 里更早就返回了，实际不会走到这里
func fileOwnerUID(fs.FileInfo) (int, bool) { return 0, false }
