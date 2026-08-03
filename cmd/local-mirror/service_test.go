package main

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"local-mirror/config"
)

// TestSystemdUnitExplicitConfigPath unit 的 ExecStart 必须显式写出配置路径：
// 两个路径都是固定常量，每台机器生成的内容一致（通用模板），
// 而显式写出让 systemctl cat 自解释——比零参数信息量更大（§P1.5 ①）
func TestSystemdUnitExplicitConfigPath(t *testing.T) {
	out := systemdUnitText(serviceSpec{
		ExePath: "/usr/bin/local-mirror", ConfigPath: "/etc/local-mirror/config.yml",
		RunAsUser: "xwvike",
	})
	if !strings.Contains(out, "ExecStart=/usr/bin/local-mirror --config /etc/local-mirror/config.yml") {
		t.Errorf("ExecStart 未显式带配置路径:\n%s", out)
	}
	if !strings.Contains(out, "User=xwvike") {
		t.Errorf("系统级 unit 应有 User=:\n%s", out)
	}
	if !strings.Contains(out, "WantedBy=multi-user.target") {
		t.Errorf("系统级 unit 的 WantedBy 应为 multi-user.target:\n%s", out)
	}
}

// TestSystemdUnitUserScope 用户级 unit 不写 User=（就是当前用户），
// 且挂 default.target 而非 multi-user.target
func TestSystemdUnitUserScope(t *testing.T) {
	out := systemdUnitText(serviceSpec{
		ExePath: "/usr/bin/local-mirror", ConfigPath: "/c.yml",
		RunAsUser: "xwvike", UserScope: true,
	})
	if strings.Contains(out, "User=") {
		t.Errorf("用户级 unit 不应写 User=:\n%s", out)
	}
	if !strings.Contains(out, "WantedBy=default.target") {
		t.Errorf("用户级 unit 的 WantedBy 应为 default.target:\n%s", out)
	}
}

// TestSystemdHardeningFollowsRWPaths 有可授权路径才加固。
// 无路径时绝不能写 ProtectSystem——那会配上一个空的 ReadWritePaths，
// 服务连自己的同步根都写不了
func TestSystemdHardeningFollowsRWPaths(t *testing.T) {
	with := systemdUnitText(serviceSpec{
		ExePath: "/usr/bin/local-mirror", ConfigPath: "/c.yml",
		RWPaths: []string{"/srv/a", "/srv/b"},
	})
	if !strings.Contains(with, "ProtectSystem=full") ||
		!strings.Contains(with, "ReadWritePaths=/srv/a /srv/b") {
		t.Errorf("有同步根时应写入加固:\n%s", with)
	}

	without := systemdUnitText(serviceSpec{ExePath: "/usr/bin/local-mirror", ConfigPath: "/c.yml"})
	if strings.Contains(without, "ProtectSystem") || strings.Contains(without, "ReadWritePaths") {
		t.Errorf("无可授权路径时不应写加固（宁可明确不加固，也不要假装加固）:\n%s", without)
	}
}

// TestLaunchdPlistEscapesPaths 同步根可能含 & < > 等字符，
// 直接拼字符串会产出无法解析的 plist
func TestLaunchdPlistEscapesPaths(t *testing.T) {
	out := launchdPlistText(serviceSpec{
		ExePath: "/usr/local/bin/local-mirror",
		// 目录名里带 & 和 <
		ConfigPath: "/Users/me/A&B/<cfg>.yml",
		LogPath:    "/Users/me/Library/Logs/local-mirror.log",
	})
	if strings.Contains(out, "A&B") {
		t.Errorf("& 未被转义:\n%s", out)
	}
	if !strings.Contains(out, "A&amp;B") || !strings.Contains(out, "&lt;cfg&gt;.yml") {
		t.Errorf("路径未正确 XML 转义:\n%s", out)
	}
}

// TestLaunchdPlistParsesWithPlutil 生成的 plist 必须能被系统解析——
// 光断言字符串不够，plutil 才是权威
func TestLaunchdPlistParsesWithPlutil(t *testing.T) {
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("plutil 不可用（非 macOS）")
	}
	out := launchdPlistText(serviceSpec{
		ExePath:    "/usr/local/bin/local-mirror",
		ConfigPath: "/Users/me/A&B/config.yml",
		LogPath:    "/Users/me/Library/Logs/local-mirror.log",
	})
	f := filepath.Join(t.TempDir(), "test.plist")
	if err := os.WriteFile(f, []byte(out), 0644); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("plutil", "-lint", f).CombinedOutput(); err != nil {
		t.Fatalf("plutil 校验失败: %v\n%s\n生成内容:\n%s", err, b, out)
	}
}

// TestEnsureBlankConfigNeverOverwrites 配置是用户数据：重跑 install
// （比如为了更新加固规则）绝不能覆盖已有配置
func TestEnsureBlankConfigNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yml")

	created, err := ensureBlankConfig(path, false)
	if err != nil || !created {
		t.Fatalf("首次应创建: created=%v err=%v", created, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("配置里会写 secret，权限应为 600，实际 %o", perm)
	}

	// 用户填了内容后重跑
	const userContent = "tasks:\n  - name: mine\n    send: true\n    path: /srv/x\n"
	if err := os.WriteFile(path, []byte(userContent), 0600); err != nil {
		t.Fatal(err)
	}
	created, err = ensureBlankConfig(path, false)
	if err != nil || created {
		t.Fatalf("已存在时不应重建: created=%v err=%v", created, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != userContent {
		t.Errorf("用户配置被覆盖了:\n%s", got)
	}
}

// TestRWPathsFromConfigSkipsCriticalRoot 任一任务的根落在关键路径上就整个跳过加固：
// 与其生成一条把加固削成零、看起来却像有加固的规则，不如明确地不加固
func TestRWPathsFromConfigSkipsCriticalRoot(t *testing.T) {
	dir := t.TempDir()

	normal := filepath.Join(dir, "normal.yml")
	writeServiceCfg(t, normal, filepath.Join(dir, "data"))
	paths, note := rwPathsFromConfig(normal)
	if len(paths) != 1 || note != "" {
		t.Fatalf("常规同步根应产出授权路径: paths=%v note=%q", paths, note)
	}

	critical := filepath.Join(dir, "critical.yml")
	writeServiceCfg(t, critical, "/etc")
	paths, note = rwPathsFromConfig(critical)
	if len(paths) != 0 {
		t.Errorf("关键路径应跳过加固，实际产出 %v", paths)
	}
	if !strings.Contains(note, "关键路径") {
		t.Errorf("应说明跳过加固的原因，实际: %q", note)
	}
}

// TestRWPathsFromConfigBlankConfig 空白配置（刚 install 出来、用户还没填）
// 不该报错，只是不加固，并提示填好后重跑
func TestRWPathsFromConfigBlankConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if _, err := ensureBlankConfig(path, false); err != nil {
		t.Fatal(err)
	}
	paths, note := rwPathsFromConfig(path)
	if len(paths) != 0 {
		t.Errorf("空白配置不应产出授权路径: %v", paths)
	}
	if !strings.Contains(note, "重跑") {
		t.Errorf("应提示填好配置后重跑 install，实际: %q", note)
	}
}

// TestServiceFilePathScopes 服务描述文件的落点：用户级落 HOME 下（不需要 root），
// 系统级落系统目录。用临时 HOME 验证，不碰真实的 LaunchAgents/systemd 目录
func TestServiceFilePathScopes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	userPath, err := serviceFilePath(true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(userPath, home) {
		t.Errorf("用户级应落在 HOME 下：%s（HOME=%s）", userPath, home)
	}

	sysPath, err := serviceFilePath(false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(sysPath, home) {
		t.Errorf("系统级不应落在 HOME 下：%s", sysPath)
	}
	if !filepath.IsAbs(sysPath) {
		t.Errorf("系统级路径应为绝对路径：%s", sysPath)
	}
}

// TestInstallWritesRealFiles 真正跑一遍写盘路径（HOME 指向临时目录，
// 不触碰真实服务目录，也不调用 launchctl/systemctl）
func TestInstallWritesRealFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	svcPath, err := serviceFilePath(true)
	if err != nil {
		t.Fatal(err)
	}
	spec := serviceSpec{
		ExePath: "/usr/local/bin/local-mirror", ConfigPath: filepath.Join(home, "c.yml"),
		UserScope: true, LogPath: filepath.Join(home, "log"),
	}
	content := launchdPlistText(spec)
	if err := os.MkdirAll(filepath.Dir(svcPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svcPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(svcPath); err != nil {
		t.Fatalf("服务描述文件未落盘: %v", err)
	}
}

// TestInstallTargetUserPrefersSudoUser 系统级安装必然要 sudo，此时
// user.Current() 返回 root——直接用它会生成 User=root，而用户想要的几乎肯定是自己
// （同步根在自己家目录下，跑成 root 还会让新同步的文件变成 root 属主）
func TestInstallTargetUserPrefersSudoUser(t *testing.T) {
	t.Setenv("SUDO_USER", "xwvike")
	if got := installTargetUser(); got != "xwvike" {
		t.Errorf("应取 SUDO_USER，实际 %q", got)
	}

	// 非 sudo 场景回落到当前用户
	t.Setenv("SUDO_USER", "")
	if got := installTargetUser(); got == "" {
		t.Error("无 SUDO_USER 时应回落到当前用户，得到空值")
	}
}

// TestInstallHintFollowsConfigReadiness 配置已填好时不该再提示"配置还空着"
func TestInstallHintFollowsConfigReadiness(t *testing.T) {
	dir := t.TempDir()

	filled := filepath.Join(dir, "filled.yml")
	writeServiceCfg(t, filled, filepath.Join(dir, "data"))
	if paths, _ := rwPathsFromConfig(filled); len(paths) == 0 {
		t.Error("填好的配置应产出授权路径（收尾提示据此判断是否可启动）")
	}

	blank := filepath.Join(dir, "blank.yml")
	if _, err := ensureBlankConfig(blank, false); err != nil {
		t.Fatal(err)
	}
	if paths, _ := rwPathsFromConfig(blank); len(paths) != 0 {
		t.Error("空白配置不应产出授权路径")
	}
}

// TestServiceFileKeepsSymlinkPath 服务文件里必须写包管理器给的**稳定软链**路径，
// 而不是解析后的真实路径。
//
// brew cask 的 /opt/homebrew/bin/local-mirror 指向
// Caskroom/local-mirror/<版本>/local-mirror——把版本化路径烤进服务文件，
// 下次 brew upgrade 删掉旧 Caskroom 目录后服务就再也起不来
func TestServiceFileKeepsSymlinkPath(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "Caskroom", "9.9.9")
	if err := os.MkdirAll(real, 0755); err != nil {
		t.Fatal(err)
	}
	realBin := filepath.Join(real, "local-mirror")
	if err := os.WriteFile(realBin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(binDir, "local-mirror")
	if err := os.Symlink(realBin, link); err != nil {
		t.Skipf("本平台不支持符号链接: %v", err)
	}

	// 服务文件应引用软链本身，不含版本化的真实路径
	out := systemdUnitText(serviceSpec{ExePath: link, ConfigPath: "/etc/local-mirror/config.yml"})
	if !strings.Contains(out, link) {
		t.Errorf("应写入软链路径 %s:\n%s", link, out)
	}
	if strings.Contains(out, "Caskroom/9.9.9") {
		t.Errorf("不应出现版本化真实路径（brew upgrade 后会失效）:\n%s", out)
	}
}

// TestProcdInitScript OpenWrt 的 procd init 脚本必须符合 rc.common 规范：
// shebang 指向 /etc/rc.common、USE_PROCD=1、start_service 里成对开关 instance
func TestProcdInitScript(t *testing.T) {
	out := procdInitScript(serviceSpec{
		ExePath: "/usr/bin/local-mirror", ConfigPath: "/etc/local-mirror/config.yml",
	})
	for _, want := range []string{
		"#!/bin/sh /etc/rc.common",
		"USE_PROCD=1",
		"start_service()",
		"procd_open_instance",
		"procd_set_param command /usr/bin/local-mirror --config /etc/local-mirror/config.yml",
		"procd_set_param respawn",
		"procd_close_instance",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("procd 脚本缺少 %q:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(out, "#!/bin/sh /etc/rc.common") {
		t.Errorf("shebang 必须在第一行:\n%s", out)
	}
	// procd 的 set_param 不认 user/group（实测 OpenWrt 24.10），
	// 写了也会被忽略，反而让人误以为降权生效了
	if strings.Contains(out, "procd_set_param user") || strings.Contains(out, "procd_set_param group") {
		t.Errorf("procd 不支持 user/group，不应写入:\n%s", out)
	}
	// systemd 专属的东西不能漏进来
	for _, forbidden := range []string{"ExecStart", "ProtectSystem", "WantedBy"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("procd 脚本混入了 systemd 指令 %q:\n%s", forbidden, out)
		}
	}
}

// TestProcdScriptIsShellValid 生成的脚本必须是合法 shell 语法——
// 它是要被 /bin/sh 执行的，语法错会让服务完全起不来
func TestProcdScriptIsShellValid(t *testing.T) {
	out := procdInitScript(serviceSpec{
		ExePath: "/usr/bin/local-mirror", ConfigPath: "/etc/local-mirror/config.yml",
	})
	f := filepath.Join(t.TempDir(), "init")
	if err := os.WriteFile(f, []byte(out), 0755); err != nil {
		t.Fatal(err)
	}
	// -n 只做语法检查不执行（rc.common 在非 OpenWrt 上不存在，不能真跑）
	if b, err := exec.Command("sh", "-n", f).CombinedOutput(); err != nil {
		t.Fatalf("shell 语法检查失败: %v\n%s\n生成内容:\n%s", err, b, out)
	}
}

// TestRunAsRejectedForUserScope 用户级服务必然以你自己的身份运行，
// 给了 --run-as 必须报错而不是静默忽略——静默忽略旗子最让人困惑
func TestRunAsRejectedForUserScope(t *testing.T) {
	if _, err := resolveRunAsUser("someone", true, ""); err == nil {
		t.Error("用户级 + --run-as 应报错")
	}
	if got, err := resolveRunAsUser("", true, ""); err != nil || got != "" {
		t.Errorf("用户级不指定身份应返回空: %q %v", got, err)
	}
}

// TestRunAsValidatesUser 写进 unit 前必须确认用户存在，
// 否则装得上、起不来，报的还是跟同步毫无关系的错
func TestRunAsValidatesUser(t *testing.T) {
	if _, err := resolveRunAsUser("definitely-no-such-user-xyz", false, ""); err == nil {
		t.Error("不存在的用户应被拒绝")
	}
	cur, err := user.Current()
	if err != nil {
		t.Skip("取不到当前用户")
	}
	if got, err := resolveRunAsUser(cur.Username, false, ""); err != nil || got != cur.Username {
		t.Errorf("存在的用户应通过: %q %v", got, err)
	}
}

// TestRunAsRoundTripsThroughGeneratedFiles 重装时要能读回已安装服务的运行身份，
// 否则重跑一次 install 就把它悄悄换掉了（同步目录属主会突然对不上）。
// 用「生成 → 解析」闭环测：两个平台的产物各自都要能被自己的解析函数读回来
func TestRunAsRoundTripsThroughGeneratedFiles(t *testing.T) {
	spec := serviceSpec{ExePath: "/usr/bin/local-mirror", ConfigPath: "/c.yml",
		LogPath: "/l", RunAsUser: "someuser"}

	if got := runAsFromSystemdUnit(systemdUnitText(spec)); got != "someuser" {
		t.Errorf("systemd unit 应读回 someuser，实际 %q", got)
	}
	if got := runAsFromPlist(launchdPlistText(spec)); got != "someuser" {
		t.Errorf("launchd plist 应读回 someuser，实际 %q", got)
	}

	// 没写运行身份（以 root 跑）时应读到空，不能瞎猜出一个来
	bare := serviceSpec{ExePath: "/usr/bin/local-mirror", ConfigPath: "/c.yml", LogPath: "/l"}
	if got := runAsFromSystemdUnit(systemdUnitText(bare)); got != "" {
		t.Errorf("无 User= 时应为空，实际 %q", got)
	}
	if got := runAsFromPlist(launchdPlistText(bare)); got != "" {
		t.Errorf("无 UserName 时应为空，实际 %q", got)
	}
}

// TestLaunchdUserNameOnlyForSystemScope LaunchAgent 必然以登录用户跑，
// 写 UserName 无意义；LaunchDaemon 才需要
func TestLaunchdUserNameOnlyForSystemScope(t *testing.T) {
	agent := launchdPlistText(serviceSpec{
		ExePath: "/x", ConfigPath: "/c.yml", LogPath: "/l", UserScope: true, RunAsUser: "someone",
	})
	if strings.Contains(agent, "UserName") {
		t.Errorf("LaunchAgent 不应写 UserName:\n%s", agent)
	}
	daemon := launchdPlistText(serviceSpec{
		ExePath: "/x", ConfigPath: "/c.yml", LogPath: "/l", RunAsUser: "someone",
	})
	if !strings.Contains(daemon, "<key>UserName</key>") || !strings.Contains(daemon, "<string>someone</string>") {
		t.Errorf("LaunchDaemon 应写 UserName:\n%s", daemon)
	}
}

// TestChownConfigToCurrentUser chown 到当前用户是幂等的空操作（属主已正确）
func TestChownConfigToCurrentUser(t *testing.T) {
	cur, err := user.Current()
	if err != nil {
		t.Skip("取不到当前用户")
	}
	path := filepath.Join(t.TempDir(), "config.yml")
	if _, err := ensureBlankConfig(path, false); err != nil {
		t.Fatal(err)
	}
	if err := chownConfigTo(path, cur.Username); err != nil {
		t.Errorf("chown 给当前用户应成功: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("chown 不应改变权限位，实际 %o", perm)
	}
}

// TestBlankConfigTemplateIsValidYAML 空白模板必须是合法 YAML：
// 它同时被 service install 写盘、被 deb/rpm 投递到 /etc，
// 语法坏掉会让用户一上来就撞解析错误
func TestBlankConfigTemplateIsValidYAML(t *testing.T) {
	if !strings.HasPrefix(blankConfigTemplate, "# local-mirror") {
		t.Fatalf("embed 的模板内容异常: %.60s", blankConfigTemplate)
	}
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(blankConfigTemplate), 0600); err != nil {
		t.Fatal(err)
	}
	// 全是注释 → 合法 YAML 但没有任务，应报「tasks 为空」而非解析错误
	_, err := config.LoadMultiConfig(path)
	if err == nil {
		t.Fatal("空白模板不该产出可用配置")
	}
	if strings.Contains(err.Error(), "failed to parse YAML") {
		t.Fatalf("空白模板不是合法 YAML: %v", err)
	}
}

func writeServiceCfg(t *testing.T, cfgPath, syncRoot string) {
	t.Helper()
	if err := os.MkdirAll(syncRoot, 0755); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	body := "tasks:\n  - name: t\n    send: true\n    path: " + syncRoot + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
