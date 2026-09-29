package tree

import (
	"io/fs"
	"os"
	"runtime"
)

// dirOwnerBits 汇端目录恒保留的属主 rwx：源端的只读目录（如 Go 模块缓存的 0555）
// 若原样套到汇端，后续子项就写不进去了。只收紧 group/other，保密语义不打折
const dirOwnerBits = 0o700

// PermOf 取条目的权限位（rwx×3，不含 setuid/setgid/sticky）。
// Windows 的权限位只是只读属性的投影，没有可同步的语义：返回 0 表示"未知"，
// 两端都不据此改权限、比对或计入 rollup
func PermOf(fi fs.FileInfo) uint32 {
	if runtime.GOOS == "windows" {
		return 0
	}
	return uint32(fi.Mode().Perm())
}

// EffectiveMode 汇端落地后应呈现的权限（比对与 rollup 都以它为准）：
// 未知仍为 0；目录并上 dirOwnerBits
func EffectiveMode(isDir bool, mode uint32) uint32 {
	if mode == 0 {
		return 0
	}
	if isDir {
		return mode | dirOwnerBits
	}
	return mode
}

// ApplyPerm 把上游权限落到本地路径。未知权限或 Windows 上不做任何事
func ApplyPerm(path string, isDir bool, mode uint32) error {
	m := EffectiveMode(isDir, mode)
	if m == 0 || runtime.GOOS == "windows" {
		return nil
	}
	return os.Chmod(path, os.FileMode(m))
}
