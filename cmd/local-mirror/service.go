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
	"strconv"
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

// systemdQuote 把一个字面量编码成 systemd 设置里安全的单个 token（SVC-01）。
// systemd 会按空白切分参数、对 % 做 specifier 展开；命令行还会对 $ 做变量展开。
// 含空格/引号/反斜杠/%/$ 的路径若不编码，会拆断 ExecStart、截断 ReadWritePaths、
// 或被误当作 specifier/变量。统一双引号包裹并转义。escapeDollar 仅命令行（ExecStart）
// 需要——ReadWritePaths 不做变量展开，传 false 以免把字面 $ 变成 $$。
func systemdQuote(s string, escapeDollar bool) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "%", "%%")
	if escapeDollar {
		s = strings.ReplaceAll(s, "$", "$$")
	}
	return `"` + s + `"`
}

// shSingleQuote 用 POSIX 单引号安全包裹一个字面量，供写进 procd 的 sh 脚本（SVC-01）。
// 单引号内除单引号外一切都是字面量；单引号本身用 '\” 收尾-转义-续起。不这样处理，
// 含空格的路径会 word-split 成多参数，含 ;/$()/反引号 的路径存在 shell 注入面。
func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

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
	fmt.Fprintf(&b, "ExecStart=%s --config %s\n", systemdQuote(s.ExePath, true), systemdQuote(s.ConfigPath, true))
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5s\n")
	b.WriteString("NoNewPrivileges=true\n")
	if s.Harden() {
		b.WriteString("ProtectSystem=full\n")
		quoted := make([]string, len(s.RWPaths))
		for i, p := range s.RWPaths {
			quoted[i] = systemdQuote(p, false)
		}
		fmt.Fprintf(&b, "ReadWritePaths=%s\n", strings.Join(quoted, " "))
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
	// LaunchDaemon（系统级）才有换用户一说；LaunchAgent 必然以登录用户身份跑
	if !s.UserScope && s.RunAsUser != "" {
		fmt.Fprintf(&b, "\t<key>UserName</key>\n\t<string>%s</string>\n\n", esc(s.RunAsUser))
	}
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n\n")
	// 优雅退出（exit 0）不重启，异常退出才拉起——与 systemd 的 Restart=on-failure 对齐
	b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n\n")
	fmt.Fprintf(&b, "\t<key>StandardOutPath</key>\n\t<string>%s</string>\n", esc(s.LogPath))
	fmt.Fprintf(&b, "\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", esc(s.LogPath))
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// init 系统标识。Linux 上不止 systemd——OpenWrt 用的是 procd，
// 两者的服务描述文件格式、落点、注册方式全不一样
const (
	initSystemd = "systemd"
	initProcd   = "procd"
	initLaunchd = "launchd"
)

// detectInit 判断本机的 init 系统。
//
// /run/systemd/system 是「以 systemd 引导」的权威判据（比 systemctl 是否在
// PATH 里可靠——容器/chroot 里可能装了工具却不是 systemd 引导）。
// 都不匹配时回落到 systemd，保持既有行为
func detectInit() string {
	if runtime.GOOS == "darwin" {
		return initLaunchd
	}
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return initSystemd
	}
	if _, err := os.Stat("/sbin/procd"); err == nil {
		return initProcd
	}
	return initSystemd
}

// procdInitScript 生成 OpenWrt 的 procd init 脚本。
//
// ⚠️ procd 的 _procd_set_param 不支持 user/group（实测 OpenWrt 24.10 的
// /lib/functions/procd.sh 只认 command/respawn/stdout/stderr/no_new_privs 等），
// 所以 procd 下服务只能以 root 运行，也没有 ProtectSystem/ReadWritePaths 的对应物。
// 能给的加固只有 no_new_privs
func procdInitScript(s serviceSpec) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh /etc/rc.common\n")
	b.WriteString("# local-mirror directory sync —— 由 `local-mirror service install` 生成\n\n")
	b.WriteString("START=95\n")
	b.WriteString("STOP=10\n")
	b.WriteString("USE_PROCD=1\n\n")
	b.WriteString("start_service() {\n")
	b.WriteString("\tprocd_open_instance\n")
	fmt.Fprintf(&b, "\tprocd_set_param command %s --config %s\n", shSingleQuote(s.ExePath), shSingleQuote(s.ConfigPath))
	b.WriteString("\tprocd_set_param respawn\n")
	// 让横幅与错误进 logread，OpenWrt 上没有 journalctl
	b.WriteString("\tprocd_set_param stdout 1\n")
	b.WriteString("\tprocd_set_param stderr 1\n")
	b.WriteString("\tprocd_set_param no_new_privs 1\n")
	b.WriteString("\tprocd_close_instance\n")
	b.WriteString("}\n")
	return b.String()
}

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
	runAs := fs.String("run-as", "", "with --system: run the service as this user (default: keep the installed one, else the invoking user)")
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
		serviceInstall(scopeIsUser, *configPath, *runAs, *dryRun)
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
	fmt.Fprintf(w, "  --run-as     with --system: run the service as this user. Reinstalling keeps the\n")
	fmt.Fprintf(w, "               user already installed, so it is never changed behind your back;\n")
	fmt.Fprintf(w, "               otherwise defaults to the invoking user. The config is chowned to\n")
	fmt.Fprintf(w, "               them (still 0600) so the service can read it\n")
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

func serviceInstall(userScope bool, explicitConfig, explicitRunAs string, dryRun bool) {
	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: cannot determine own executable path: %v\n", err)
		os.Exit(1)
	}
	// ⚠️ 刻意不做 filepath.EvalSymlinks：包管理器给的正是一个**稳定软链**，
	// 解析后会得到带版本号的真实路径（brew cask 是
	// /opt/homebrew/bin/local-mirror → Caskroom/local-mirror/<版本>/local-mirror）。
	// 把版本化路径烤进服务文件，下次 brew upgrade 删掉旧 Caskroom 目录后
	// 服务就再也起不来了。软链本身才是该写进去的长期有效路径

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

	// 运行身份要在 svcPath 之后定：重装时得先能读到已安装服务里的既有身份
	runAsUser, err := resolveRunAsUser(explicitRunAs, userScope, svcPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
		os.Exit(2)
	}
	spec := serviceSpec{
		ExePath: exePath, ConfigPath: cfgPath,
		UserScope: userScope, RWPaths: rwPaths, RunAsUser: runAsUser,
	}

	var content string
	switch detectInit() {
	case initLaunchd:
		home, _ := os.UserHomeDir()
		spec.LogPath = filepath.Join(home, "Library", "Logs", "local-mirror.log")
		content = launchdPlistText(spec)
	case initProcd:
		content = procdInitScript(spec)
	default:
		content = systemdUnitText(spec)
	}

	if dryRun {
		fmt.Printf("[dry-run] 将写入服务描述文件 %s（运行身份 %s）：\n\n%s\n",
			svcPath, runAsDesc(runAsUser, userScope), content)
		reportConfigOutcome(cfgPath, created, dryRun)
		if runAsUser != "" {
			fmt.Printf("[dry-run] 将把配置交给 %s（chown，权限仍保持 600）\n", runAsUser)
		}
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
	if err := os.WriteFile(svcPath, []byte(content), serviceFileMode()); err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: cannot write %s: %v\n(系统级安装需要 root，试试 sudo)\n", svcPath, err)
		os.Exit(1)
	}
	fmt.Printf("服务描述文件已写入 %s（运行身份 %s）\n", svcPath, runAsDesc(runAsUser, userScope))

	// 配置是 0600，属主不对运行用户就读不到、服务起不来。改属主而非放宽权限
	if runAsUser != "" {
		if err := chownConfigTo(cfgPath, runAsUser); err != nil {
			fmt.Fprintf(os.Stderr,
				"警告：无法把配置交给 %s（%v）。服务以该用户运行时读不到 0600 的配置会起不来，请手工执行：\n  sudo chown %s %s\n",
				runAsUser, err, runAsUser, cfgPath)
		}
	}

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
	// 配置是否已经可用，决定收尾提示该说"先去编辑"还是"可以启动了"。
	// rwPathsFromConfig 拿得到授权路径 ⇔ 配置解析成功且有任务
	if len(rwPaths) > 0 {
		fmt.Printf("\n下一步：启动服务\n")
	} else {
		fmt.Printf("\n下一步：编辑配置后再启动（配置还没有可用任务，现在启动必然失败）\n")
	}
	fmt.Printf("  %s\n", startHint(userScope))
	// 学 xray 的 systemd_cat_config：让用户能核对**合并 drop-in 之后**的实际生效内容，
	// 而不是只知道"文件写到哪了"
	if effective := effectiveConfigHint(userScope); effective != "" {
		// systemd 的 cat 会把 drop-in 合并进来，procd/launchd 没有这个概念
		label := "核对实际生效的服务配置"
		if detectInit() == initSystemd {
			label += "（含 drop-in）"
		}
		fmt.Printf("\n%s：\n  %s\n", label, effective)
	}
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
