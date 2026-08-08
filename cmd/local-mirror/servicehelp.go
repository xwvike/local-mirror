package main

import (
	"fmt"
	"local-mirror/config"
	"local-mirror/internal/safety"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// rwPathsFromConfig 从配置里算出需要授权可写的同步根。
//
// 返回空切片 = 不加固。三种情况都会落到这里：配置还是空白的（首次安装）、
// 配置解析不了、或任一任务的根落在关键路径上（此时授权范围会大到让加固失去意义）
func rwPathsFromConfig(configPath string) (paths []string, note string) {
	cfg, err := config.LoadMultiConfig(configPath)
	if err != nil {
		// procd 本就没有 ProtectSystem/ReadWritePaths 的对应物，
		// 在那里承诺「重跑即可补上加固」是空头支票
		if detectInit() == initProcd {
			return nil, ""
		}
		return nil, "配置尚未填写或暂不可解析：本次不写入 ProtectSystem 加固；填好配置后重跑 service install 即可补上"
	}
	for _, t := range cfg.Tasks {
		if critical, hit := safety.IsCriticalRoot(t.Path); critical {
			return nil, fmt.Sprintf("任务 %q 的同步根 %s 落在关键路径（%s）上：本次不写入 ProtectSystem 加固（授权范围会大到让加固失去意义）", t.Name, t.Path, hit)
		}
		paths = append(paths, t.Path)
	}
	return paths, ""
}

// serviceFilePath 返回服务描述文件应落的位置
func serviceFilePath(userScope bool) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if userScope {
			return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"), nil
		}
		return filepath.Join("/Library", "LaunchDaemons", serviceLabel+".plist"), nil
	case "linux":
		if detectInit() == initProcd {
			// procd 没有用户级服务的概念
			if userScope {
				return "", fmt.Errorf("procd (OpenWrt) has no per-user services; install with --system")
			}
			return "/etc/init.d/local-mirror", nil
		}
		if userScope {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			return filepath.Join(home, ".config", "systemd", "user", serviceUnitName), nil
		}
		return filepath.Join("/etc/systemd/system", serviceUnitName), nil
	default:
		return "", fmt.Errorf("service management is not supported on %s yet", runtime.GOOS)
	}
}

// serviceFileMode 服务描述文件的权限。procd 的 init 脚本是要被执行的，必须可执行；
// systemd unit 与 launchd plist 只是被读取
func serviceFileMode() os.FileMode {
	if detectInit() == initProcd {
		return 0755
	}
	return 0644
}

// defaultUserScope 各平台的默认作用域。
// Linux 服务的常态是系统级 unit；macOS 上守护用户自己的目录，
// LaunchAgent（用户级）才是常态，而且不需要 root
func defaultUserScope() bool { return runtime.GOOS == "darwin" }

// resolveServiceConfigPath 未显式给 --config 时取平台约定落点
func resolveServiceConfigPath(explicit string, userScope bool) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	if userScope {
		return config.UserConfigPath()
	}
	return config.SystemConfigPath()
}

// runAsDesc 运行身份的人类可读描述
func runAsDesc(runAsUser string, userScope bool) string {
	switch {
	case userScope:
		return "当前用户"
	case runAsUser == "":
		return "root"
	default:
		return runAsUser
	}
}

// effectiveConfigHint 查看合并后生效配置的命令
func effectiveConfigHint(userScope bool) string {
	switch runtime.GOOS {
	case "linux":
		if detectInit() == initProcd {
			return "cat /etc/init.d/local-mirror   # 日志：logread -e local-mirror"
		}
		if userScope {
			return "systemctl --user cat local-mirror"
		}
		return "systemctl cat local-mirror"
	case "darwin":
		if userScope {
			return fmt.Sprintf("launchctl print gui/%d/%s", os.Getuid(), serviceLabel)
		}
		return fmt.Sprintf("sudo launchctl print system/%s", serviceLabel)
	}
	return ""
}

// installTargetUser 系统级 unit 里 User= 的兜底人选。
//
// 系统级安装必然要 sudo，此时 user.Current() 返回的是 root——直接用它会生成
// User=root，而用户想要的几乎肯定是自己（同步根在自己家目录下，跑成 root
// 还会让新同步下来的文件变成 root 属主）。SUDO_USER 才是真正的调用者
func installTargetUser() string {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		return sudoUser
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// resolveRunAsUser 定下系统级服务以谁的身份运行。
//
// 优先级：显式 --run-as ＞ 已安装服务里的既有身份 ＞ SUDO_USER/当前用户。
// 中间那档是关键：重跑 install（比如为了更新加固规则）**不能悄悄换掉运行身份**，
// 否则同步目录的属主会突然对不上。用户级服务没有这个概念——它必然以你自己跑，
// 此时给了 --run-as 直接报错，而不是静默忽略一个旗子
func resolveRunAsUser(explicit string, userScope bool, svcPath string) (string, error) {
	if userScope {
		if explicit != "" {
			return "", fmt.Errorf("--run-as 只对 --system 安装有意义：用户级服务必然以你自己的身份运行")
		}
		return "", nil
	}
	// procd 的 procd_set_param 不认 user/group，服务只能以 root 跑。
	// 与其生成一个被忽略的参数、让用户以为降权生效了，不如直接拒绝
	if detectInit() == initProcd {
		if explicit != "" {
			return "", fmt.Errorf("procd (OpenWrt) 不支持服务降权，--run-as 无法生效；服务将以 root 运行")
		}
		return "", nil
	}
	if explicit != "" {
		if _, err := user.Lookup(explicit); err != nil {
			return "", fmt.Errorf("用户 %q 在本机不存在：%w", explicit, err)
		}
		return explicit, nil
	}
	if existing := existingRunAsUser(svcPath); existing != "" {
		if _, err := user.Lookup(existing); err == nil {
			return existing, nil // 重装：沿用既有身份
		}
	}
	fallback := installTargetUser()
	if fallback == "" {
		return "", nil // 交给 systemd 默认（root）
	}
	if _, err := user.Lookup(fallback); err != nil {
		return "", fmt.Errorf("用户 %q 在本机不存在（可用 --run-as 显式指定）：%w", fallback, err)
	}
	return fallback, nil
}

// existingRunAsUser 读出已安装服务当前的运行身份，读不到返回空。
//
// Linux 上优先问 systemd 要**合并后的生效值**——它能看到 drop-in 里的 User=，
// 而只 grep 主 unit 文件会漏掉（我们自己的 debian 部署正是把 User= 写在 drop-in 里）
func existingRunAsUser(svcPath string) string {
	if runtime.GOOS == "linux" {
		out, err := exec.Command("systemctl", "show", serviceUnitName, "-p", "User", "--value").Output()
		if v := strings.TrimSpace(string(out)); err == nil && v != "" {
			return v
		}
	}
	data, err := os.ReadFile(svcPath)
	if err != nil {
		return ""
	}
	if runtime.GOOS == "darwin" {
		return runAsFromPlist(string(data))
	}
	return runAsFromSystemdUnit(string(data))
}

// runAsFromSystemdUnit 从 unit 文本里取 User=。纯函数，便于各平台测
func runAsFromSystemdUnit(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "User="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// runAsFromPlist 取 <key>UserName</key> 后面紧跟的那个 <string>。纯函数
func runAsFromPlist(text string) string {
	i := strings.Index(text, "<key>UserName</key>")
	if i < 0 {
		return ""
	}
	rest := text[i:]
	s := strings.Index(rest, "<string>")
	if s < 0 {
		return ""
	}
	e := strings.Index(rest[s:], "</string>")
	if e < 0 {
		return ""
	}
	return strings.TrimSpace(rest[s+len("<string>") : s+e])
}

// chownConfigTo 把配置交给运行用户。
//
// 配置是 0600（里面有 secret），属主不对就读不到、服务起不来。改属主而非放宽
// 权限：密钥的暴露面一点不扩大，只是从"仅 root 可读"变成"仅运行用户可读"
func chownConfigTo(cfgPath, username string) error {
	if username == "" {
		return nil // 以 root 跑，无需改
	}
	u, err := user.Lookup(username)
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(cfgPath); err == nil {
		if owner, ok := fileOwnerUID(fi); ok && owner == uid {
			return nil // 属主已正确，不必（也可能没权限）再 chown 一次
		}
	}
	return os.Chown(cfgPath, uid, gid)
}

// registerCmd 注册服务的命令。launchd 的 bootstrap 需要域名 + plist 路径；
// systemd 只需 daemon-reload（enable/start 交给用户，见 install 的收尾提示）
func registerCmd(userScope bool, svcPath string) []string {
	switch runtime.GOOS {
	case "darwin":
		if userScope {
			return []string{"launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), svcPath}
		}
		return []string{"launchctl", "bootstrap", "system", svcPath}
	case "linux":
		if detectInit() == initProcd {
			// procd 没有 daemon-reload，enable 建好 rc.d 软链即完成注册
			return []string{svcPath, "enable"}
		}
		if userScope {
			return []string{"systemctl", "--user", "daemon-reload"}
		}
		return []string{"systemctl", "daemon-reload"}
	}
	return nil
}

func startHint(userScope bool) string {
	switch runtime.GOOS {
	case "darwin":
		if userScope {
			return fmt.Sprintf("launchctl kickstart -k gui/%d/%s", os.Getuid(), serviceLabel)
		}
		return fmt.Sprintf("sudo launchctl kickstart -k system/%s", serviceLabel)
	case "linux":
		if detectInit() == initProcd {
			return "/etc/init.d/local-mirror start"
		}
		if userScope {
			return "systemctl --user enable --now local-mirror"
		}
		return "sudo systemctl enable --now local-mirror"
	}
	return ""
}

// ensureBlankConfig 建目录与空白配置。已存在则原样保留——
// 配置是用户数据，重跑 install（比如为了更新加固规则）绝不能覆盖它
func ensureBlankConfig(path string, dryRun bool) (created bool, err error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("cannot inspect %s: %w", path, err)
	}
	if dryRun {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, fmt.Errorf("cannot create %s: %w", filepath.Dir(path), err)
	}
	// 0600：配置里会写 secret
	if err := os.WriteFile(path, []byte(blankConfigTemplate), 0600); err != nil {
		return false, fmt.Errorf("cannot write %s: %w", path, err)
	}
	return true, nil
}

func reportConfigOutcome(path string, created, dryRun bool) {
	switch {
	case created && dryRun:
		fmt.Printf("[dry-run] 将创建空白配置 %s（600）\n", path)
	case created:
		fmt.Printf("已创建空白配置 %s（600）——编辑它\n", path)
	default:
		fmt.Printf("配置已存在，原样保留：%s\n", path)
	}
}

func deregisterCmd(userScope bool, svcPath string) []string {
	switch runtime.GOOS {
	case "darwin":
		if userScope {
			return []string{"launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), serviceLabel)}
		}
		return []string{"launchctl", "bootout", "system/" + serviceLabel}
	case "linux":
		if detectInit() == initProcd {
			return []string{svcPath, "disable"}
		}
		if userScope {
			return []string{"systemctl", "--user", "disable", "--now", serviceUnitName}
		}
		return []string{"systemctl", "disable", "--now", serviceUnitName}
	}
	return nil
}
