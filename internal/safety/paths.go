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
	// 一切非相对形式直接拒绝。仅查 IsAbs 在 Windows 上不够："/x"、"\x"
	// （rooted，锚定到当前盘根）与 "C:x"（盘符相对）都不算 IsAbs，却携带
	// 越界语义；合法的协议相对路径永远不会以分隔符或盘符开头，
	// 为跨平台行为一致，三类形式在所有平台上一律拒绝
	if filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" ||
		strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return "", fmt.Errorf("路径越界（非相对路径）: %q", rel)
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

// 关键路径分两类，语义不同：
//
// criticalSubtrees：系统管理的目录树，**其内部任意子目录**都算关键路径。
// 这是三级阶梯最初要防护的主场景——`-p /etc/nginx` 与 `-p /etc` 同样危险，
// 覆盖的都是系统配置。
//
// criticalRootsOnly："容器"目录，只有**本身**算关键路径，子目录是正常的
// 用户工作区。例如 `-p ~` 会镜像掉整个家目录（危险），但 `-p ~/Pictures`
// 是这个工具最日常的用法，不该要求旗子。`/` 必须归此类：所有路径都是
// `/` 的子目录，若按子树处理则一切路径都成关键路径，判定退化为恒真。
func criticalSubtrees() []string {
	switch runtime.GOOS {
	case "darwin":
		// /var 不入子树：macOS 的用户临时目录（$TMPDIR）就在 /var/folders 下
		return []string{
			"/System", "/Library", "/Applications",
			"/bin", "/sbin", "/usr", "/etc",
		}
	case "windows":
		return []string{
			`C:\Windows`, `C:\Program Files`, `C:\Program Files (x86)`,
			`C:\ProgramData`,
		}
	default: // linux 及其它类 Unix
		return []string{
			"/bin", "/sbin", "/usr", "/etc", "/var", "/lib", "/lib64",
			"/boot", "/sys", "/proc", "/dev", "/run",
		}
	}
}

func criticalRootsOnly() []string {
	var paths []string

	// 用户主目录本身（跨平台）；子目录是日常同步目标，不受限
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, home)
	}

	switch runtime.GOOS {
	case "darwin":
		paths = append(paths,
			"/", "/Users", "/private", "/var", "/opt", "/cores", "/Volumes",
		)
	case "windows":
		paths = append(paths, `C:\`, `C:\Users`)
	default: // linux 及其它类 Unix
		paths = append(paths,
			"/", "/home", "/root", "/opt", "/srv", "/mnt", "/media",
		)
	}
	return paths
}

// normalize 取路径的真实绝对形式：解引用符号链接后再清洗。
// 必须解引用，否则可以用 `ln -s /etc alias` 之类的方式绕过关键路径校验。
//
// 路径尚不存在时，EvalSymlinks 会整体失败，但前缀里的符号链接依然必须
// 解析——macOS 上 /etc 是指向 /private/etc 的链接，`-p /etc/尚未创建的目录`
// 若不解析前缀就匹配不到关键路径。因此逐级回退：解析最深的已存在祖先，
// 再把不存在的尾部拼回去。
func normalize(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	abs = filepath.Clean(abs)

	suffix := ""
	cur := abs
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Clean(filepath.Join(resolved, suffix))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs // 连根都解析不了，退回 Abs+Clean（尽力而为）
		}
		suffix = filepath.Join(filepath.Base(cur), suffix)
		cur = parent
	}
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

// IsCriticalRoot 判断同步根是否落在关键路径上。命中返回 true 及命中的关键路径。
// 校验对象是解引用后的真实路径，防止符号链接绕过。
//
// 系统目录树（criticalSubtrees）双向检测：同步根包含它（如 -p / 覆盖 /etc）
// 或落在它内部（如 -p /etc/nginx）都算命中——后者是真实网络测试中发现的
// 判定缺口（原实现只查了前一个方向）。
// 容器目录（criticalRootsOnly）只检测"同步根等于或包含它"：其子目录
// （~/Pictures、/Volumes/Backup/x 等）是正常工作区，不受限。
func IsCriticalRoot(syncRoot string) (bool, string) {
	root := normalize(syncRoot)
	for _, danger := range criticalSubtrees() {
		d := normalize(danger)
		if isAncestorOrEqual(root, d) || isAncestorOrEqual(d, root) {
			return true, d
		}
	}
	for _, danger := range criticalRootsOnly() {
		d := normalize(danger)
		if isAncestorOrEqual(root, d) {
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
