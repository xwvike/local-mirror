package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"local-mirror/config"
	"local-mirror/internal/keyfile"
	"local-mirror/internal/safety"

	"gopkg.in/yaml.v3"
)

// serviceOnlyFlags service 子命令自己的旗子（值 = 是否带参数）。其余参数都按前台
// 运行的旗子解析，翻译成配置里的一个任务——前台试跑的命令前加上 service install
// 就成了常驻服务
var serviceOnlyFlags = map[string]bool{
	"system": false, "user": false, "config": true, "run-as": true, "dry-run": false,
	"h": false, "help": false,
}

// splitServiceArgs 把 service 自己的旗子与前台运行参数分开，两者可任意穿插
func splitServiceArgs(args []string) (svcArgs, runArgs []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		takesVal, isSvc := serviceOnlyFlags[name]
		if !strings.HasPrefix(a, "-") || !isSvc {
			runArgs = append(runArgs, a)
			continue
		}
		svcArgs = append(svcArgs, a)
		if takesVal && !hasVal && i+1 < len(args) {
			i++
			svcArgs = append(svcArgs, args[i])
		}
	}
	return svcArgs, runArgs
}

// serviceTask 由前台运行参数翻译出的任务，以及安装完成后要给用户看的 key 信息
type serviceTask struct {
	task     config.TaskConfig
	shownKey string // --gen-key 得到的 key，安装后在终端里打印（连同对端命令）
}

// taskFromRunFlags 解析前台运行参数并翻译成一个任务。existing 是现有配置内容
// （可能为空），用于 --gen-key 沿用同一目录上已有的 key
func taskFromRunFlags(runArgs []string, existing []byte) (*serviceTask, error) {
	if err := flag.CommandLine.Parse(runArgs); err != nil {
		return nil, err
	}
	positionalArgs = parseInterspersed()
	if err := resolveDirection(); err != nil {
		return nil, err
	}
	set := cliFlagsSet()
	if !*config.SendFlag && !*config.ReceiveFlag && !set["m"] && !set["mode"] {
		return nil, fmt.Errorf("a direction is required: --send and/or --receive")
	}
	for _, name := range []string{"status", "heat", "all", "show-key", "secret-stdin", "version", "v"} {
		if set[name] {
			return nil, fmt.Errorf("--%s is not a sync option and cannot be installed as a service", name)
		}
	}
	if *config.NoEncrypt {
		return nil, fmt.Errorf("--no-encrypt is not supported for services; the link requires a key (--gen-key or -k <key>)")
	}
	if err := config.ValidateRuntimeNumbers(); err != nil {
		return nil, err
	}
	root, err := resolveSyncRoot()
	if err != nil {
		return nil, err
	}
	config.StartPath = root

	t := config.TaskConfig{Name: *config.Alias, Path: root}
	switch *config.Mode {
	case "reality":
		t.Send = true
	case "mirror":
		t.Receive = true
	case "relay":
		t.Send, t.Receive = true, true
	}
	if config.SinkListens {
		t.Listen = true
	} else {
		t.Connect = *config.RealityIP
	}
	if *config.Ignore != "" {
		for _, p := range strings.Split(*config.Ignore, ",") {
			if p = strings.TrimSpace(p); p != "" {
				t.Ignore = append(t.Ignore, p)
			}
		}
	}
	if set["l"] || set["loglevel"] {
		t.LogLevel = *config.LogLevel
	}
	if set["c"] || set["cooldown"] {
		t.CoolDown = *config.CoolDown
	}
	if set["f"] || set["filebuffersize"] {
		t.FileBufferSize = *config.FileBufferSize
	}
	t.AllowDelete = *config.AllowDelete
	t.AllowCritical = *config.AllowCritical

	// 接收端落在关键路径上却没解锁：服务起来就会以 exit 2 退出，装的时候就拦下
	if t.Receive {
		if _, err := safety.CheckSyncSafety(root, t.AllowCritical); err != nil {
			return nil, err
		}
	}

	st := &serviceTask{task: t}
	switch {
	case *config.Secret != "":
		st.task.Secret = *config.Secret
	case *config.GenKey:
		// 沿用已有 key：同步根里的 key 文件（前台试跑时生成、对端已拿到的那把）＞
		// 配置里同一目录任务的 secret ＞ 新生成。--force 才换新 key
		key := ""
		if !*config.Force {
			if k, err := keyfile.Load(root); err == nil {
				key = k
			}
			if key == "" {
				key = existingTaskSecret(existing, root)
			}
		}
		if key == "" {
			if key, err = keyfile.NewKey(); err != nil {
				return nil, err
			}
		}
		st.task.Secret = key
		st.shownKey = key
	}

	// 监听端不能明文：服务起来会被拒绝启动（exit 2），装的时候就拦下。
	// key 可以来自本任务的 secret、配置的 defaults.secret，或同步根里的 key 文件
	listens := t.Listen || (t.Send && (t.Receive || t.Connect == ""))
	if listens && st.task.Secret == "" && existingDefaultSecret(existing) == "" {
		if k, _ := keyfile.Load(root); k == "" {
			return nil, fmt.Errorf("a listening end requires a key: add --gen-key or -k <key>")
		}
	}
	return st, nil
}

// lenientConfig 宽松解析现有配置（不校验），只为读出已有的 key
func lenientConfig(existing []byte) config.MultiConfig {
	var cfg config.MultiConfig
	_ = yaml.Unmarshal(existing, &cfg)
	return cfg
}

func existingTaskSecret(existing []byte, root string) string {
	for _, t := range lenientConfig(existing).Tasks {
		if abs, err := filepath.Abs(t.Path); err == nil && abs == root {
			return t.Secret
		}
	}
	return ""
}

func existingDefaultSecret(existing []byte) string {
	return lenientConfig(existing).Defaults.Secret
}

// upsertTaskYAML 把任务写进配置：同一同步根的任务就地替换，否则追加。
// 现有内容里的注释尽量保留——只有注释的空白模板直接在末尾追加 tasks 段
// （yaml 节点树解析纯注释文档会丢掉全部注释）
func upsertTaskYAML(existing []byte, t config.TaskConfig) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(existing, &doc); err != nil {
		return nil, fmt.Errorf("existing config is not valid YAML: %w", err)
	}
	if doc.Kind == 0 {
		block, err := encodeYAML(&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			strNode("tasks"), {Kind: yaml.SequenceNode, Content: []*yaml.Node{taskNode(t)}},
		}})
		if err != nil {
			return nil, err
		}
		out := append([]byte{}, existing...)
		if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
			out = append(out, '\n')
		}
		if len(out) > 0 {
			out = append(out, '\n')
		}
		return append(out, block...), nil
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("existing config is not a YAML mapping")
	}
	var tasks *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "tasks" {
			tasks = root.Content[i+1]
		}
	}
	if tasks == nil || tasks.Kind != yaml.SequenceNode {
		tasks = &yaml.Node{Kind: yaml.SequenceNode}
		root.Content = append(root.Content, strNode("tasks"), tasks)
	}
	item := taskNode(t)
	replaced := false
	for i, n := range tasks.Content {
		if taskNodePath(n) == t.Path {
			item.HeadComment = n.HeadComment
			tasks.Content[i] = item
			replaced = true
			break
		}
	}
	if !replaced {
		tasks.Content = append(tasks.Content, item)
	}
	return encodeYAML(&doc)
}

// taskNodePath 取任务节点的 path（解析成绝对路径），用于判断是否同一同步根
func taskNodePath(n *yaml.Node) string {
	if n.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "path" {
			if abs, err := filepath.Abs(n.Content[i+1].Value); err == nil {
				return abs
			}
		}
	}
	return ""
}

// taskNode 按固定顺序、只写非零字段地生成任务节点，让写出的配置读起来与手写一致
func taskNode(t config.TaskConfig) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode}
	add := func(k string, v *yaml.Node) { n.Content = append(n.Content, strNode(k), v) }
	boolTrue := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"}
	if t.Name != "" {
		add("name", strNode(t.Name))
	}
	if t.Send {
		add("send", boolTrue)
	}
	if t.Receive {
		add("receive", boolTrue)
	}
	if t.Connect != "" {
		add("connect", strNode(t.Connect))
	}
	if t.Listen {
		add("listen", boolTrue)
	}
	add("path", strNode(t.Path))
	if t.AllowDelete {
		add("allow_delete", boolTrue)
	}
	if t.AllowCritical {
		add("allow_critical", boolTrue)
	}
	if len(t.Ignore) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, p := range t.Ignore {
			seq.Content = append(seq.Content, strNode(p))
		}
		add("ignore", seq)
	}
	if t.Secret != "" {
		add("secret", strNode(t.Secret))
	}
	if t.LogLevel != "" {
		add("loglevel", strNode(t.LogLevel))
	}
	if t.CoolDown != 0 {
		add("cooldown", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(t.CoolDown, 10)})
	}
	if t.FileBufferSize != 0 {
		add("filebuffersize", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatUint(t.FileBufferSize, 10)})
	}
	return n
}

func strNode(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }

func encodeYAML(n *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// removeTaskYAML 从配置里删掉同步根为 root 的任务（其余内容与注释保留）。
// 返回删除后剩余的任务数；removed=false 表示配置里没有这个目录
func removeTaskYAML(existing []byte, root string) (out []byte, removed bool, left int, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(existing, &doc); err != nil {
		return nil, false, 0, fmt.Errorf("config is not valid YAML: %w", err)
	}
	if doc.Kind == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return existing, false, 0, nil
	}
	m := doc.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != "tasks" || m.Content[i+1].Kind != yaml.SequenceNode {
			continue
		}
		seq := m.Content[i+1]
		kept := seq.Content[:0]
		for _, n := range seq.Content {
			if taskNodePath(n) == root {
				removed = true
				continue
			}
			kept = append(kept, n)
		}
		seq.Content = kept
		left = len(kept)
	}
	if !removed {
		return existing, false, left, nil
	}
	out, err = encodeYAML(&doc)
	return out, true, left, err
}

// configState 服务配置的状态。区分"还没填任务"与"填错了"：前者是刚装完的正常状态，
// 后者必须把解析错误摆到用户面前，不能当成空白配置悄悄放过
type configState int

const (
	cfgMissing configState = iota // 文件不存在
	cfgBlank                      // 存在但还没有任务（空白模板、tasks: []）
	cfgBroken                     // 有错（语法、未知字段、校验不过）或读不了
	cfgReady                      // 可用
)

func inspectConfig(path string) (configState, *config.MultiConfig, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfgMissing, nil, nil
	}
	if err != nil {
		return cfgBroken, nil, err
	}
	cfg, perr := config.ParseMultiConfig(data, path)
	if perr == nil {
		return cfgReady, cfg, nil
	}
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) == nil && (doc.Kind == 0 || len(lenientConfig(data).Tasks) == 0) {
		return cfgBlank, nil, perr
	}
	return cfgBroken, nil, perr
}

// describeTask 任务的方向与对端，一行人读描述（作用于 ParseMultiConfig 归一后的任务）
func describeTask(t config.TaskConfig) string {
	peer := t.RealityIP
	if peer == "" {
		peer = "局域网发现"
	}
	switch t.Mode {
	case "relay":
		return "中继 ← " + peer + "，同时监听"
	case "reality":
		if t.RealityIP != "" {
			return "发送端 → " + t.RealityIP
		}
		return "发送端，监听"
	default:
		if t.Listen {
			return "接收端，监听"
		}
		return "接收端 ← " + peer
	}
}

// redactedTaskYAML dry-run 展示用：隐去 secret
func redactedTaskYAML(t config.TaskConfig) string {
	if t.Secret != "" {
		t.Secret = "<hidden>"
	}
	out, err := encodeYAML(&yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{taskNode(t)}})
	if err != nil {
		return ""
	}
	return string(out)
}

// startStep 启动服务的一步；mayFail 的步骤失败不算错（如先卸下一个本就没加载的 launchd 任务）。
// waitGone 非空时，执行后等到 `launchctl print <waitGone>` 查不到该任务为止：bootout 是
// 异步的，旧任务还没拆完就 bootstrap 会报 "Bootstrap failed: 5: Input/output error"
type startStep struct {
	args     []string
	mayFail  bool
	waitGone string
}

// startSteps 注册并启动（已在运行则重启，让新配置生效）
func startSteps(userScope bool, svcPath string) []startStep {
	switch runtime.GOOS {
	case "darwin":
		domain, target := "system", "system/"+serviceLabel
		if userScope {
			domain = fmt.Sprintf("gui/%d", os.Getuid())
			target = domain + "/" + serviceLabel
		}
		// plist 可能已变：卸下旧的再按新文件加载，RunAtLoad 随即拉起
		return []startStep{
			{args: []string{"launchctl", "bootout", target}, mayFail: true, waitGone: target},
			{args: []string{"launchctl", "bootstrap", domain, svcPath}},
		}
	case "linux":
		if detectInit() == initProcd {
			return []startStep{{args: []string{svcPath, "enable"}}, {args: []string{svcPath, "restart"}}}
		}
		sc := []string{"systemctl"}
		if userScope {
			sc = append(sc, "--user")
		}
		return []startStep{
			{args: append(append([]string{}, sc...), "daemon-reload")},
			{args: append(append([]string{}, sc...), "enable", serviceUnitName)},
			{args: append(append([]string{}, sc...), "restart", serviceUnitName)},
		}
	}
	return nil
}

// waitLaunchdGone 等 launchd 任务彻底卸下（最多 10 秒）
func waitLaunchdGone(target string) {
	for i := 0; i < 50; i++ {
		if exec.Command("launchctl", "print", target).Run() != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// isServiceRunning 服务当前是否在运行。刚启动时应先等片刻再查：进程若因配置错误
// 退出，要能看出来
func isServiceRunning(userScope bool) bool {
	switch runtime.GOOS {
	case "darwin":
		target := "system/" + serviceLabel
		if userScope {
			target = fmt.Sprintf("gui/%d/%s", os.Getuid(), serviceLabel)
		}
		out, err := exec.Command("launchctl", "print", target).Output()
		return err == nil && strings.Contains(string(out), "state = running")
	case "linux":
		if detectInit() == initProcd {
			out, err := exec.Command("/etc/init.d/local-mirror", "status").CombinedOutput()
			return err == nil && strings.Contains(string(out), "running")
		}
		args := []string{"is-active", "--quiet", serviceUnitName}
		if userScope {
			args = append([]string{"--user"}, args...)
		}
		return exec.Command("systemctl", args...).Run() == nil
	}
	return false
}

// logHint 服务日志在哪里看
func logHint(userScope bool) string {
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		return "tail -n 30 " + filepath.Join(home, "Library", "Logs", "local-mirror.log")
	case "linux":
		if detectInit() == initProcd {
			return "logread -e local-mirror"
		}
		if userScope {
			return "journalctl --user -u local-mirror -n 30"
		}
		// 系统日志对普通用户是否可读因发行版而异，sudo 处处可用
		return "sudo journalctl -u local-mirror -n 30"
	}
	return ""
}
