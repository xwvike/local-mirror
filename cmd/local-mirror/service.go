package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"local-mirror/config"
)

// 服务标识。两个平台各按自己的惯例：systemd 用 unit 文件名，launchd 用反向域名 label
const (
	serviceUnitName = "local-mirror.service"
	serviceLabel    = "com.xwvike.local-mirror"
)

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

// registerCmd 注册服务的命令。launchd 的 bootstrap 需要域名 + plist 路径；
// systemd 只需 daemon-reload（enable/start 交给用户，见 install 的收尾提示）

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
