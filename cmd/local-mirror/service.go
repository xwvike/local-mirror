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
	"time"

	"local-mirror/config"
	"local-mirror/internal/keyfile"

	"golang.org/x/term"
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
	svcArgs, runArgs := splitServiceArgs(args)
	_ = fs.Parse(svcArgs)
	if len(runArgs) > 0 && action != "install" && action != "remove" {
		fmt.Fprintf(os.Stderr, "local-mirror: service %s takes no sync options, got %v\n", action, runArgs)
		os.Exit(2)
	}

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
		serviceInstall(scopeIsUser, *configPath, *runAs, *dryRun, runArgs, false)
	case "restart":
		serviceInstall(scopeIsUser, *configPath, *runAs, *dryRun, nil, true)
	case "remove":
		serviceRemove(scopeIsUser, *configPath, *dryRun, runArgs)
	case "uninstall":
		serviceUninstall(scopeIsUser, *dryRun)
	case "status":
		serviceStatus(scopeIsUser, *configPath)
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
	fmt.Fprintf(w, "Usage: local-mirror service install [flags] [sync options]\n")
	fmt.Fprintf(w, "       local-mirror service remove -p <dir> [flags]\n")
	fmt.Fprintf(w, "       local-mirror service <status|restart|uninstall> [flags]\n\n")
	fmt.Fprintf(w, "  install      install a foreground command as a long-running service:\n")
	fmt.Fprintf(w, "                 sudo local-mirror service install <the same options>   (Linux)\n")
	fmt.Fprintf(w, "                 local-mirror service install <the same options>        (macOS)\n")
	fmt.Fprintf(w, "               The sync options (--send/--receive/--connect/\n")
	fmt.Fprintf(w, "               --listen/-p/--gen-key/-k/...) become a task in the service config\n")
	fmt.Fprintf(w, "               (same sync root: replaced; otherwise: added; other content kept);\n")
	fmt.Fprintf(w, "               then the service file is written, registered and (re)started.\n")
	fmt.Fprintf(w, "               Without sync options: create a blank config for manual editing;\n")
	fmt.Fprintf(w, "               local-mirror service restart then starts the service\n")
	fmt.Fprintf(w, "  status       show the config file, its tasks, the service state and the\n")
	fmt.Fprintf(w, "               management commands\n")
	fmt.Fprintf(w, "  restart      apply a manually edited config: validate it, regenerate the service\n")
	fmt.Fprintf(w, "               file and restart. A config with errors is rejected without changes;\n")
	fmt.Fprintf(w, "               the running service is not affected\n")
	fmt.Fprintf(w, "  remove       remove one directory's task from the config and restart; removing\n")
	fmt.Fprintf(w, "               the last task stops and uninstalls the service (the config is kept)\n")
	fmt.Fprintf(w, "  uninstall    stop and deregister the service, remove its description file.\n")
	fmt.Fprintf(w, "               The config file is always kept\n\n")
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

// serviceInstall 安装常驻服务。runArgs 非空时（与前台运行相同的参数），先把它翻译成
// 配置里的一个任务（同一同步根就地替换，否则追加），再写服务文件、注册并启动——
// 前台试跑的命令前加上 service install 就成了常驻服务。runArgs 为空时按现有配置
// 安装：没有配置就建空白配置、注册但不启动；配置可用就启动。
// restart 为真（service restart）：只按现有配置重新生成服务文件并重启，配置缺失、
// 没有任务或有错都报错退出，不动正在运行的服务
func serviceInstall(userScope bool, explicitConfig, explicitRunAs string, dryRun bool, runArgs []string, restart bool) {
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

	var st *serviceTask
	var cfg *config.MultiConfig
	var cfgErr error
	created := false
	if len(runArgs) > 0 {
		existing, rerr := os.ReadFile(cfgPath)
		if rerr != nil && !os.IsNotExist(rerr) {
			fmt.Fprintf(os.Stderr, "local-mirror: cannot read %s: %v\n", cfgPath, rerr)
			os.Exit(1)
		}
		if st, err = taskFromRunFlags(runArgs, existing); err != nil {
			fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
			os.Exit(2)
		}
		base := existing
		if os.IsNotExist(rerr) {
			base, created = []byte(blankConfigTemplate), true
		}
		newData, uerr := upsertTaskYAML(base, st.task)
		if uerr != nil {
			fmt.Fprintf(os.Stderr, "local-mirror: %v\n", uerr)
			os.Exit(1)
		}
		// 写盘前按真实落点校验整份配置：装出一个起不来的服务不如当场报错
		if cfg, cfgErr = config.ParseMultiConfig(newData, cfgPath); cfgErr != nil {
			fmt.Fprintf(os.Stderr, "local-mirror: the resulting config would be invalid: %v\n", cfgErr)
			os.Exit(2)
		}
		if dryRun {
			fmt.Printf("[dry-run] 将把以下任务写入 %s（600）：\n%s\n", cfgPath, redactedTaskYAML(st.task))
		} else {
			if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
				fmt.Fprintf(os.Stderr, "local-mirror: cannot create %s: %v\n", filepath.Dir(cfgPath), err)
				os.Exit(1)
			}
			if err := os.WriteFile(cfgPath, newData, 0600); err != nil {
				fmt.Fprintf(os.Stderr, "local-mirror: cannot write %s: %v\n(系统级安装需要 root 权限，请使用 sudo)\n", cfgPath, err)
				os.Exit(1)
			}
			_ = os.Chmod(cfgPath, 0600) // WriteFile 不改已有文件的权限
			fmt.Printf("任务已写入配置 %s（同步根 %s）\n", cfgPath, st.task.Path)
		}
	} else {
		state, c, perr := inspectConfig(cfgPath)
		switch state {
		case cfgBroken:
			// 手改出错：摆出具体错误，不碰服务——正在运行的实例仍按旧配置工作
			fmt.Fprintf(os.Stderr, "local-mirror: 配置有错，未做任何改动（正在运行的服务不受影响）：\n  %s\n  %v\n", cfgPath, perr)
			os.Exit(2)
		case cfgMissing:
			if restart {
				fmt.Fprintf(os.Stderr, "local-mirror: 服务配置不存在（%s），请先执行 %slocal-mirror service install <前台运行参数>\n", cfgPath, sudoPrefix(userScope))
				os.Exit(1)
			}
			// 目录与空白配置由我们建，用户只需要编辑
			if created, err = ensureBlankConfig(cfgPath, dryRun); err != nil {
				fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
				os.Exit(1)
			}
			cfgErr = fmt.Errorf("no tasks in config")
		case cfgBlank:
			if restart {
				fmt.Fprintf(os.Stderr, "local-mirror: 配置 %s 中没有任务，无可重启的服务\n", cfgPath)
				os.Exit(2)
			}
			cfgErr = perr
		case cfgReady:
			cfg = c
		}
	}
	ready := cfgErr == nil && cfg != nil && len(cfg.Tasks) > 0
	rwPaths, note := rwPathsFrom(cfg, cfgErr)

	if runtime.GOOS == "windows" {
		// Windows 原生服务要接 SCM，是独立的一块工作量，本期明确不做。
		// 但配置目录与配置仍然照建——宁可少做并说清楚，
		// 也不要生成一个装上去跑不起来的东西
		if len(runArgs) == 0 {
			reportConfigOutcome(cfgPath, created, dryRun)
		}
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
	if restart {
		if _, err := os.Stat(svcPath); err != nil {
			fmt.Fprintf(os.Stderr, "local-mirror: 服务未安装（%s），请先执行 %slocal-mirror service install <前台运行参数>\n", svcPath, sudoPrefix(userScope))
			os.Exit(1)
		}
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
		if len(runArgs) == 0 {
			reportConfigOutcome(cfgPath, created, dryRun)
		}
		if runAsUser != "" {
			fmt.Printf("[dry-run] 将把配置交给 %s（chown，权限仍保持 600）\n", runAsUser)
		}
		if note != "" {
			fmt.Printf("提示：%s\n", note)
		}
		if ready {
			for _, step := range startSteps(userScope, svcPath) {
				fmt.Printf("[dry-run] 将执行：%s\n", strings.Join(step.args, " "))
			}
		} else if args := registerCmd(userScope, svcPath); len(args) > 0 {
			fmt.Printf("[dry-run] 将执行：%s\n", strings.Join(args, " "))
		}
		return
	}

	if err := os.MkdirAll(filepath.Dir(svcPath), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: cannot create %s: %v\n", filepath.Dir(svcPath), err)
		os.Exit(1)
	}
	if err := os.WriteFile(svcPath, []byte(content), serviceFileMode()); err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: cannot write %s: %v\n(系统级安装需要 root 权限，请使用 sudo)\n", svcPath, err)
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
	if len(runArgs) == 0 && !restart {
		reportConfigOutcome(cfgPath, created, dryRun)
	}
	if note != "" {
		fmt.Printf("提示：%s\n", note)
	}

	if !ready {
		// 配置还没有可用任务：只注册、不启动（macOS 上 registerCmd 为空，见其注释）
		if args := registerCmd(userScope, svcPath); len(args) > 0 {
			if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "警告：注册命令失败（%v）：%s\n手工执行：%s\n",
					err, strings.TrimSpace(string(out)), strings.Join(args, " "))
			}
		}
		fmt.Printf("\n下一步：在 %s 中填写任务，然后执行 %slocal-mirror service restart 注册并启动服务\n", cfgPath, sudoPrefix(userScope))
		return
	}

	failed := false
	for _, step := range startSteps(userScope, svcPath) {
		if out, err := exec.Command(step.args[0], step.args[1:]...).CombinedOutput(); err != nil && !step.mayFail {
			fmt.Fprintf(os.Stderr, "启动失败：%s（%v）：%s\n", strings.Join(step.args, " "), err, strings.TrimSpace(string(out)))
			failed = true
			break
		}
		if step.waitGone != "" {
			waitLaunchdGone(step.waitGone)
		}
	}
	if !failed {
		time.Sleep(3 * time.Second)
	}
	if failed || !isServiceRunning(userScope) {
		fmt.Fprintf(os.Stderr, "\n服务未能运行。日志：\n  %s\n", logHint(userScope))
		os.Exit(1)
	}
	fmt.Printf("服务已启动并在运行（%d 个任务）\n", len(cfg.Tasks))
	fmt.Printf("\n任务与管理命令：local-mirror service status\n")
	fmt.Printf("同步状态：local-mirror --status --all    日志：%s\n", logHint(userScope))

	if st != nil && st.shownKey != "" {
		fmt.Printf("\nkey fingerprint: %s\n", keyfile.Fingerprint(st.shownKey))
		if term.IsTerminal(int(os.Stdout.Fd())) {
			fmt.Printf("key:             %s\n\n", st.shownKey)
			label, cmd := peerKeyHint(st.shownKey)
			fmt.Printf("%s\n  %s\n", label, cmd)
		} else {
			fmt.Printf("(key not shown: stdout is not a terminal; it is in %s)\n", cfgPath)
		}
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
		fmt.Fprintf(os.Stderr, "local-mirror: cannot remove %s: %v\n(系统级卸载需要 root 权限，请使用 sudo)\n", svcPath, err)
		os.Exit(1)
	}
	fmt.Printf("服务已卸载：%s\n", svcPath)
	fmt.Printf("配置文件已保留，未删除\n")
}

// serviceStatus 服务的总入口：配置在哪、有哪些任务、服务是否在运行、怎么修改
func serviceStatus(userScope bool, explicitConfig string) {
	scope := "system"
	if userScope {
		scope = "user"
	}
	fmt.Printf("平台     %s (%s scope)\n", runtime.GOOS, scope)

	cfgPath, err := resolveServiceConfigPath(explicitConfig, userScope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
		os.Exit(1)
	}
	state, cfg, cerr := inspectConfig(cfgPath)
	switch state {
	case cfgMissing:
		fmt.Printf("配置     %s（不存在）\n", cfgPath)
	case cfgBlank:
		fmt.Printf("配置     %s（还没有任务）\n", cfgPath)
	case cfgBroken:
		if os.IsPermission(cerr) {
			fmt.Printf("配置     %s（无读取权限，需使用 sudo）\n", cfgPath)
		} else {
			fmt.Printf("配置     %s（有错：%v）\n", cfgPath, cerr)
		}
	case cfgReady:
		fmt.Printf("配置     %s（%d 个任务）\n", cfgPath, len(cfg.Tasks))
	}

	svcPath, err := serviceFilePath(userScope)
	if err != nil {
		fmt.Printf("服务     本平台暂不支持服务管理\n")
		return
	}
	svcState := "未安装"
	if _, err := os.Stat(svcPath); err == nil {
		svcState = "已安装，未运行"
		if isServiceRunning(userScope) {
			svcState = "已安装，运行中"
		}
	}
	fmt.Printf("服务     %s（%s）\n", svcPath, svcState)

	if cfg != nil {
		fmt.Printf("\n任务\n")
		for _, t := range cfg.Tasks {
			fmt.Printf("  %s %s %s\n", padCell(t.Name, 14), padCell(describeTask(t), 36), t.Path)
		}
	}

	sp := sudoPrefix(userScope)
	fmt.Printf("\n管理\n")
	fmt.Printf("  修改或新增目录：%slocal-mirror service install <前台运行参数>\n", sp)
	if sp != "" && runtime.GOOS == "linux" {
		fmt.Printf("  手工编辑配置：sudoedit %s，然后执行 %slocal-mirror service restart\n", cfgPath, sp)
	} else {
		fmt.Printf("  手工编辑配置：编辑 %s，然后执行 %slocal-mirror service restart\n", cfgPath, sp)
	}
	fmt.Printf("  删除目录：%slocal-mirror service remove -p <目录>\n", sp)
	fmt.Printf("\n同步状态：local-mirror --status --all    日志：%s\n", logHint(userScope))
}

// sudoPrefix 系统级服务的管理命令要 root
func sudoPrefix(userScope bool) string {
	if userScope {
		return ""
	}
	return "sudo "
}

// serviceRemove 从服务配置里删掉一个同步目录的任务并重启服务；删到一个不剩就停止并
// 卸载服务（配置保留）。目录本身不必还存在
func serviceRemove(userScope bool, explicitConfig string, dryRun bool, runArgs []string) {
	if err := flag.CommandLine.Parse(runArgs); err != nil {
		os.Exit(2)
	}
	positionalArgs = parseInterspersed()
	set := cliFlagsSet()
	delete(set, "p")
	delete(set, "path")
	if len(set) > 0 || len(positionalArgs) > 0 || *config.Path == "" {
		fmt.Fprintf(os.Stderr, "local-mirror: usage: local-mirror service remove -p <dir>\n")
		os.Exit(2)
	}
	root, err := filepath.Abs(*config.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
		os.Exit(2)
	}
	cfgPath, err := resolveServiceConfigPath(explicitConfig, userScope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
		os.Exit(1)
	}
	data, err := os.ReadFile(cfgPath)
	if os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "local-mirror: 服务配置不存在（%s），没有可删除的任务\n", cfgPath)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: cannot read %s: %v\n", cfgPath, err)
		os.Exit(1)
	}
	out, removed, left, err := removeTaskYAML(data, root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: %v\n", err)
		os.Exit(2)
	}
	if !removed {
		fmt.Fprintf(os.Stderr, "local-mirror: 配置中没有同步目录为 %s 的任务（现有任务见 local-mirror service status）\n", root)
		os.Exit(2)
	}
	if left > 0 {
		if _, err := config.ParseMultiConfig(out, cfgPath); err != nil {
			fmt.Fprintf(os.Stderr, "local-mirror: 删除后的配置有错，未做任何改动：%v\n", err)
			os.Exit(2)
		}
	}
	if dryRun {
		fmt.Printf("[dry-run] 将从 %s 删除同步目录 %s 的任务（剩 %d 个）\n", cfgPath, root, left)
		return
	}
	if err := os.WriteFile(cfgPath, out, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "local-mirror: cannot write %s: %v\n", cfgPath, err)
		os.Exit(1)
	}
	fmt.Printf("已从配置删除：%s\n", root)

	svcPath, err := serviceFilePath(userScope)
	if err != nil {
		return
	}
	if _, err := os.Stat(svcPath); err != nil {
		fmt.Printf("服务未安装，仅修改了配置\n")
		return
	}
	if left == 0 {
		fmt.Printf("配置中已无任务，停止并卸载服务\n")
		serviceUninstall(userScope, false)
		return
	}
	serviceInstall(userScope, explicitConfig, "", false, nil, true)
}
