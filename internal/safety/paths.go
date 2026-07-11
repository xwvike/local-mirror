// Package safety 提供针对危险操作（尤其是删除）的路径安全校验。
package safety

import (
	"fmt"
	"io"
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

// IsCriticalRoot 判断同步根是否落在关键路径上（相等，或是某关键路径的祖先，
// 如根设为 / 或 ~）。命中返回 true 及命中的关键路径。
// 校验对象是解引用后的真实路径，防止符号链接绕过。
func IsCriticalRoot(syncRoot string) (bool, string) {
	root := normalize(syncRoot)
	for _, danger := range dangerousPaths() {
		d := normalize(danger)
		if root == d || isAncestorOrEqual(root, d) {
			return true, d
		}
	}
	return false, ""
}

// CheckSyncSafety 三级安全阶梯的启动校验（仅同步方调用）。
// 返回 snapshot 表示"覆盖已有文件前是否需要快照备份原文件"。
//   - 非关键路径：无限制、不备份，行为与历史一致。
//   - 关键路径 + 未解锁：拒绝（连只同步都不允许——同步会覆盖已存在文件）。
//   - 关键路径 + allowCritical：允许同步并开启覆盖前快照；是否删除仍由
//     --allow-delete 单独控制（两旗全给才在关键路径上删除；单给 delete 无
//     allowCritical 会在此被拒）。
func CheckSyncSafety(root string, allowCritical bool) (bool, error) {
	isCrit, hit := IsCriticalRoot(root)
	if !isCrit {
		return false, nil
	}
	if !allowCritical {
		return false, fmt.Errorf("拒绝在关键路径上同步: %s（命中 %s）。"+
			"如确需请加 --allow-critical（仅同步不删除，首次覆盖会备份原文件到 .local-mirror/backups）；"+
			"再加 --allow-delete 才会删除", normalize(root), hit)
	}
	return true, nil
}

// SnapshotBeforeOverwrite 在首次覆盖某文件前，把原文件快照到
// <root>/.local-mirror/backups/<rel>，保留 local-mirror 动手前的原始版本。
// 仅当目标已存在、且尚无快照时执行（反复同步不会 churn 掉原始版本）。
// 用整文件复制而非硬链接：硬链接与原文件共享 inode，若覆盖是原地改写
// （而非换 inode 的 rename）会连快照一起被改掉；复制的正确性不依赖覆盖方式。
// 每个文件仅在首次覆盖时复制一次，系统/配置文件通常很小，代价可接受。
// rel 由调用方保证已经过 SafeJoin 校验（在同步根内）。
func SnapshotBeforeOverwrite(root, rel, fullPath string) error {
	if _, err := os.Stat(fullPath); err != nil {
		return nil // 新文件，无需备份
	}
	backupPath := filepath.Join(root, ".local-mirror", "backups", filepath.FromSlash(rel))
	if _, err := os.Stat(backupPath); err == nil {
		return nil // 已存原始快照，绝不覆盖它
	}
	if err := os.MkdirAll(filepath.Dir(backupPath), 0755); err != nil {
		return err
	}
	return copyFile(fullPath, backupPath)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
