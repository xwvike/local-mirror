package main

import (
	"bytes"
	_ "embed"
	"encoding/xml"
	"fmt"
	"os"
	"runtime"
	"strings"
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
