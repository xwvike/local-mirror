package main

import (
	"bytes"
	_ "embed"
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"local-mirror/config"
	"local-mirror/internal/safety"
)

// 服务标识。两个平台各按自己的惯例：systemd 用 unit 文件名，launchd 用反向域名 label
const (
	serviceUnitName = "local-mirror.service"
	serviceLabel    = "com.xwvike.local-mirror"
)

// blankConfigTemplate 首次安装时创建的空白配置。全部注释掉——
// 用户取消注释填几个字段即可，不需要自己建目录、建文件、查字段名。
// 装完就能跑的前提是用户先填它，所以 install 不会顺手把服务起起来。
//
// 用 embed 而非源码里的字符串字面量：deb/rpm 也要把同一份模板投递到
// /etc/local-mirror/config.yml（见 .goreleaser.yaml），共用一个文件才不会漂移
//
//go:embed config.blank.yml
var blankConfigTemplate string

// serviceSpec 生成服务描述文件所需的全部输入。
// 抽成纯数据 + 纯函数，是为了让各平台的产物能在任一平台上被单测覆盖
type serviceSpec struct {
	ExePath    string   // 二进制绝对路径
	ConfigPath string   // 配置文件绝对路径，显式写进 ExecStart（见 §P1.5）
	RunAsUser  string   // 仅系统级 systemd 用
	UserScope  bool     // 用户级（systemd --user / launchd LaunchAgent）
	RWPaths    []string // ProtectSystem 下仍需可写的路径；空则不加固
	LogPath    string   // 仅 launchd 用
}

// Harden 是否写入 ProtectSystem/ReadWritePaths。
//
// 没有可授权路径时（配置还空着、或某个任务的根落在关键路径上）一律不加固：
// 与其生成 ReadWritePaths=/ 这种把加固削成零、看起来却像有加固的规则，
// 不如明确地不加固。见 docs/CONFIG_AND_SERVICE.md §P4.3
func (s serviceSpec) Harden() bool { return len(s.RWPaths) > 0 }

// systemdUnitText 生成 systemd unit。ExecStart 里两个路径都是固定常量，
// 每台机器生成的内容一致，是真正的通用模板
func systemdUnitText(s serviceSpec) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=local-mirror directory sync\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n\n")

	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	if !s.UserScope && s.RunAsUser != "" {
		fmt.Fprintf(&b, "User=%s\n", s.RunAsUser)
	}
	fmt.Fprintf(&b, "ExecStart=%s --config %s\n", s.ExePath, s.ConfigPath)
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5s\n")
	b.WriteString("NoNewPrivileges=true\n")
	if s.Harden() {
		b.WriteString("ProtectSystem=full\n")
		fmt.Fprintf(&b, "ReadWritePaths=%s\n", strings.Join(s.RWPaths, " "))
	}
	b.WriteString("\n[Install]\n")
	if s.UserScope {
		b.WriteString("WantedBy=default.target\n")
	} else {
		b.WriteString("WantedBy=multi-user.target\n")
	}
	return b.String()
}

// launchdPlistText 生成 launchd plist。路径全部经 XML 转义——
// 同步根可能含 & < > 等字符，直接拼字符串会产出无法解析的 plist
func launchdPlistText(s serviceSpec) string {
	esc := func(v string) string {
		var buf bytes.Buffer
		_ = xml.EscapeText(&buf, []byte(v))
		return buf.String()
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	fmt.Fprintf(&b, "\t<key>Label</key>\n\t<string>%s</string>\n\n", esc(serviceLabel))
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range []string{s.ExePath, "--config", s.ConfigPath} {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", esc(arg))
	}
	b.WriteString("\t</array>\n\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n\n")
	// 优雅退出（exit 0）不重启，异常退出才拉起——与 systemd 的 Restart=on-failure 对齐
	b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n\n")
	fmt.Fprintf(&b, "\t<key>StandardOutPath</key>\n\t<string>%s</string>\n", esc(s.LogPath))
	fmt.Fprintf(&b, "\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", esc(s.LogPath))
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// rwPathsFromConfig 从配置里算出需要授权可写的同步根。
//
// 返回空切片 = 不加固。三种情况都会落到这里：配置还是空白的（首次安装）、
// 配置解析不了、或任一任务的根落在关键路径上（此时授权范围会大到让加固失去意义）
func rwPathsFromConfig(configPath string) (paths []string, note string) {
	cfg, err := config.LoadMultiConfig(configPath)
	if err != nil {
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

// defaultUserScope 各平台的默认作用域。
// Linux 服务的常态是系统级 unit；macOS 上守护用户自己的目录，
// LaunchAgent（用户级）才是常态，而且不需要 root
func defaultUserScope() bool { return runtime.GOOS == "darwin" }

// runServiceCommand 处理 `local-mirror service <action>`，不返回。
//
// 这是项目里第一个子命令：分发发生在 flag.Parse() 之前，且只精确匹配 "service"
// 这一个词——不能用「argv[1] 不以 - 开头」来判定，那会把位置糖
// `local-mirror ./dir @peer` 里的 ./dir 误当成子命令
func runServiceCommand(args []string) {
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	systemScope := fs.Bool("system", false, "install as a system-wide service (Linux default; needs root)")
	userScope := fs.Bool("user", false, "install as a per-user service (macOS default; no root needed)")
	configPath := fs.String("config", "", "config file path (defaults to the platform's conventional location)")
	dryRun := fs.Bool("dry-run", false, "print what would be written and run, without touching the system")
	fs.Usage = func() { printServiceUsage(os.Stdout) }

	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	_ = fs.Parse(args)

	if *systemScope && *userScope {
		fmt.Fprintln(os.Stderr, "local-mirror: --system and --user are mutually exclusive")
		os.Exit(2)
	}
	scopeIsUser := defaultUserScope()
	switch {
	case *systemScope:
		scopeIsUser = false
	case *userScope:
		scopeIsUser = true
	}

	switch action {
	case "install":
		serviceInstall(scopeIsUser, *configPath, *dryRun)
	case "uninstall":
		serviceUninstall(scopeIsUser, *dryRun)
	case "status":
		serviceStatus(scopeIsUser)
	case "":
		printServiceUsage(os.Stderr)
		os.Exit(2)
	default:
		fmt.Fprintf(os.Stderr, "local-mirror: unknown service action %q\n\n", action)
		printServiceUsage(os.Stderr)
		os.Exit(2)
	}
	os.Exit(0)
}

func printServiceUsage(w *os.File) {
	fmt.Fprintf(w, "Usage: local-mirror service <install|uninstall|status> [flags]\n\n")
	fmt.Fprintf(w, "  install      create the config directory and a blank config, write the service\n")
	fmt.Fprintf(w, "               description file, and register it. Never starts the service and\n")
	fmt.Fprintf(w, "               never overwrites an existing config\n")
	fmt.Fprintf(w, "  uninstall    stop and deregister the service, remove its description file.\n")
	fmt.Fprintf(w, "               The config file is always kept\n")
	fmt.Fprintf(w, "  status       show where things are and whether the service is registered\n\n")
	fmt.Fprintf(w, "Flags:\n")
	fmt.Fprintf(w, "  --system     system-wide service (Linux default; needs root)\n")
	fmt.Fprintf(w, "  --user       per-user service (macOS default; no root needed)\n")
	fmt.Fprintf(w, "  --config     config file path (defaults to the platform's conventional location)\n")
	fmt.Fprintf(w, "  --dry-run    print what would be written and run, without touching the system\n")
}

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

func serviceInstall(userScope bool, explicitConfig string, dryRun bool) {
	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: cannot determine own executable path: %v\n", err)
		os.Exit(1)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	cfgPath, err := resolveServiceConfigPath(explicitConfig, userScope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
		os.Exit(1)
	}

	// 目录与空白配置由我们建，用户只需要编辑——这是 service install 存在的主要理由
	created, err := ensureBlankConfig(cfgPath, dryRun)
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
		os.Exit(1)
	}

	rwPaths, note := rwPathsFromConfig(cfgPath)
	spec := serviceSpec{
		ExePath: exePath, ConfigPath: cfgPath,
		UserScope: userScope, RWPaths: rwPaths,
	}
	if !userScope {
		if u, err := user.Current(); err == nil {
			spec.RunAsUser = u.Username
		}
	}

	if runtime.GOOS == "windows" {
		// Windows 原生服务要接 SCM，是独立的一块工作量，本期明确不做。
		// 但配置目录与空白配置仍然照建——宁可少做并说清楚，
		// 也不要生成一个装上去跑不起来的东西
		reportConfigOutcome(cfgPath, created, dryRun)
		fmt.Printf("\n本期尚未支持 Windows 原生服务注册。可用计划任务手工登记：\n")
		fmt.Printf("  schtasks /create /tn local-mirror /sc onstart /ru SYSTEM \\\n")
		fmt.Printf("           /tr \"%s --config %s\"\n", exePath, cfgPath)
		return
	}

	svcPath, err := serviceFilePath(userScope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
		os.Exit(1)
	}
	var content string
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		spec.LogPath = filepath.Join(home, "Library", "Logs", "local-mirror.log")
		content = launchdPlistText(spec)
	} else {
		content = systemdUnitText(spec)
	}

	if dryRun {
		fmt.Printf("[dry-run] 将写入服务描述文件 %s：\n\n%s\n", svcPath, content)
		reportConfigOutcome(cfgPath, created, dryRun)
		if note != "" {
			fmt.Printf("提示：%s\n", note)
		}
		fmt.Printf("[dry-run] 将执行：%s\n", strings.Join(registerCmd(userScope, svcPath), " "))
		return
	}

	if err := os.MkdirAll(filepath.Dir(svcPath), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: cannot create %s: %v\n", filepath.Dir(svcPath), err)
		os.Exit(1)
	}
	if err := os.WriteFile(svcPath, []byte(content), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: cannot write %s: %v\n(系统级安装需要 root，试试 sudo)\n", svcPath, err)
		os.Exit(1)
	}
	fmt.Printf("服务描述文件已写入 %s\n", svcPath)

	if args := registerCmd(userScope, svcPath); len(args) > 0 {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "警告：注册命令失败（%v）：%s\n手工执行：%s\n",
				err, strings.TrimSpace(string(out)), strings.Join(args, " "))
		}
	}

	reportConfigOutcome(cfgPath, created, dryRun)
	if note != "" {
		fmt.Printf("提示：%s\n", note)
	}
	fmt.Printf("\n下一步：编辑配置后再启动（配置还空着，现在启动必然失败）\n")
	fmt.Printf("  %s\n", startHint(userScope))
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

func serviceUninstall(userScope bool, dryRun bool) {
	svcPath, err := serviceFilePath(userScope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
		os.Exit(1)
	}
	stop := deregisterCmd(userScope, svcPath)
	if dryRun {
		fmt.Printf("[dry-run] 将执行：%s\n", strings.Join(stop, " "))
		fmt.Printf("[dry-run] 将删除：%s\n", svcPath)
		fmt.Printf("[dry-run] 配置文件保留不动\n")
		return
	}
	if len(stop) > 0 {
		// 服务可能本来就没在跑，注销失败不算错误
		_, _ = exec.Command(stop[0], stop[1:]...).CombinedOutput()
	}
	if err := os.Remove(svcPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "local-mirror: cannot remove %s: %v\n(系统级卸载需要 root，试试 sudo)\n", svcPath, err)
		os.Exit(1)
	}
	fmt.Printf("服务已卸载：%s\n", svcPath)
	fmt.Printf("配置文件保留不动（它是你的数据，需要时请手工删除）\n")
}

func deregisterCmd(userScope bool, svcPath string) []string {
	switch runtime.GOOS {
	case "darwin":
		if userScope {
			return []string{"launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), serviceLabel)}
		}
		return []string{"launchctl", "bootout", "system/" + serviceLabel}
	case "linux":
		if userScope {
			return []string{"systemctl", "--user", "disable", "--now", serviceUnitName}
		}
		return []string{"systemctl", "disable", "--now", serviceUnitName}
	}
	return nil
}

func serviceStatus(userScope bool) {
	scope := "system"
	if userScope {
		scope = "user"
	}
	fmt.Printf("平台     %s (%s scope)\n", runtime.GOOS, scope)

	cfgPath, err := resolveServiceConfigPath("", userScope)
	if err == nil {
		state := "缺失"
		if _, err := os.Stat(cfgPath); err == nil {
			state = "存在"
			if _, loadErr := config.LoadMultiConfig(cfgPath); loadErr != nil {
				state = "存在但未填写/不可解析"
			}
		}
		fmt.Printf("配置     %s (%s)\n", cfgPath, state)
	}

	svcPath, err := serviceFilePath(userScope)
	if err != nil {
		fmt.Printf("服务     本平台暂不支持服务管理\n")
		return
	}
	state := "未安装"
	if _, err := os.Stat(svcPath); err == nil {
		state = "已安装"
	}
	fmt.Printf("服务     %s (%s)\n", svcPath, state)
	fmt.Printf("\n运行态观测：local-mirror --status --all\n")
}
