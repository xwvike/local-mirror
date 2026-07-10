// Package safety 提供针对危险操作（尤其是删除）的路径安全校验。
package safety

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// SafeJoin 把服务端下发的相对路径 rel 安全地拼到同步根 root 下。
// rel 完全来自对端（不可信输入）：必须确保拼接、清洗后的最终路径仍落在
// root 之内，否则一个 "../../etc/x" 之类的路径会逃出同步根，造成同步目录
// 外的任意文件写入/删除。命中越界返回错误，调用方应拒绝该项而非落盘。
//
// 校验基于词法清洗（Clean），不依赖磁盘状态，因此对尚不存在的目标路径同样
// 有效；root 自身允许（rel 为 "." 或 ""）。
func SafeJoin(root, rel string) (string, error) {
	// 绝对路径直接拒绝：拼接绝对路径会丢弃 root
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("路径越界（绝对路径）: %q", rel)
	}
	cleanRoot := filepath.Clean(root)
	joined := filepath.Clean(filepath.Join(cleanRoot, rel))
	if joined == cleanRoot {
		return joined, nil
	}
	// 必须是 root 的真子路径：以 root+分隔符 为前缀
	if !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界（逃出同步根 %s）: %q", cleanRoot, rel)
	}
	return joined, nil
}

// dangerousPaths 返回当前平台上不应作为"可删除同步根目录"的关键路径列表。
// 这些是操作系统或用户的核心目录，在其上启用删除极易造成灾难性数据损失。
// 列表按需持续补充。
func dangerousPaths() []string {
	var paths []string

	// 用户主目录本身（跨平台）
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, home)
	}

	switch runtime.GOOS {
	case "darwin":
		paths = append(paths,
			"/", "/System", "/Library", "/Applications", "/Users",
			"/bin", "/sbin", "/usr", "/etc", "/var", "/private",
			"/opt", "/cores", "/Volumes",
		)
	case "windows":
		paths = append(paths,
			`C:\`, `C:\Windows`, `C:\Program Files`, `C:\Program Files (x86)`,
			`C:\Users`, `C:\ProgramData`,
		)
	default: // linux 及其它类 Unix
		paths = append(paths,
			"/", "/bin", "/sbin", "/usr", "/etc", "/var", "/lib", "/lib64",
			"/boot", "/sys", "/proc", "/dev", "/root", "/opt", "/srv", "/run",
			"/home", "/mnt", "/media",
		)
	}
	return paths
}

// normalize 取路径的真实绝对形式：解引用符号链接后再清洗。
// 必须解引用，否则可以用 `ln -s /etc alias` 之类的方式绕过关键路径校验。
// 若路径不存在或无法解引用，退回到 Abs+Clean（尽力而为）。
func normalize(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		if abs, err := filepath.Abs(resolved); err == nil {
			return filepath.Clean(abs)
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

// isAncestorOrEqual 判断 ancestor 是否等于 target，或是 target 的祖先目录。
// 例如 ancestor="/" 或 "~" 会覆盖 "/etc"、系统目录等，同样危险。
func isAncestorOrEqual(ancestor, target string) bool {
	if ancestor == target {
		return true
	}
	rel, err := filepath.Rel(ancestor, target)
	if err != nil {
		return false
	}
	// target 在 ancestor 之下：rel 不以 ".." 开头、也不是绝对路径
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(rel)
}

// CheckDeletableRoot 校验同步根目录能否安全地在其上启用删除。
// 命中关键路径（相等，或该根目录是某个关键路径的祖先）时返回错误。
// 校验对象是解引用后的真实路径，防止符号链接绕过。
func CheckDeletableRoot(syncRoot string) error {
	root := normalize(syncRoot)
	for _, danger := range dangerousPaths() {
		d := normalize(danger)
		// 根目录 == 关键路径，或根目录是关键路径的祖先（如根设为 / 或 ~）
		if root == d || isAncestorOrEqual(root, d) {
			return fmt.Errorf("拒绝在关键路径上启用删除同步: %s（命中 %s）", root, d)
		}
	}
	return nil
}
