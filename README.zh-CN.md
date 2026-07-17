# local-mirror

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/wordmark-dark.svg">
    <img src="assets/wordmark.svg" width="480" alt="LOCAL-MIRROR">
  </picture>
</p>

[English](README.md) | 简体中文

基于 TCP 的单向目录镜像。一台机器共享目录（`reality` 模式），其他机器保持
它的实时副本（`mirror`），也可以经中继级联成 A → B → C 传递链。各平台都是
单个静态二进制。

```
┌─────────────┐    目录树 / 变更 / 文件    ┌─────────────┐
│   reality   │ ─────────────────────────▶ │   mirror    │
│  （服务端）  │       自定义 TCP 协议      │  （客户端）  │
└─────────────┘                            └─────────────┘
  监听文件变化                                持续拉取，保持一致
```

镜像默认只做增量：客户端本地多出来的文件不会被动，删除必须显式加参数。
文件按 blake3 哈希比对，分块传输、原子落盘，中断的下载可以续传。变更通常
两秒左右到达镜像端，默认每 30 分钟一次的全量扫描兜底丢失的通知。符号链接
不同步也不解引用。

## 安装

macOS：

```bash
brew install xwvike/tap/local-mirror
```

Windows，经 [Scoop](https://scoop.sh)（没装 Scoop 的话 `irm get.scoop.sh | iex`
先装它，不需要管理员权限）：

```powershell
scoop bucket add xwvike https://github.com/xwvike/scoop-bucket
scoop install local-mirror
```

Linux（任意发行版；macOS 不想用 Homebrew 也能用）：

```bash
curl -fsSL https://raw.githubusercontent.com/xwvike/local-mirror/main/install.sh | sh
```

脚本自动识别系统与架构，下载最新版本并校验 checksum。root 运行装
`/usr/local/bin`；普通用户装 `~/.local/bin`（需要时自动补 PATH），
绝不索要提权。环境变量 `VERSION` 和 `INSTALL_DIR` 可覆盖默认行为。
想走正经包管理的话，[releases 页面](https://github.com/xwvike/local-mirror/releases)
上有 deb 和 rpm。

或从源码构建：

```bash
git clone https://github.com/xwvike/local-mirror && cd local-mirror
go build -o local-mirror ./cmd/local-mirror
```

## 快速开始

```bash
# 服务端：共享指定目录（-p 省略时为当前工作目录）
local-mirror -m reality -p /path/to/source

# 另一台机器上镜像它
local-mirror -m mirror -r 192.168.1.100 -p /path/to/replica

# 不指定 -r：自动发现局域网服务端并交互选择
local-mirror -m mirror -p /path/to/replica

# 中继：从上游镜像下来，同时向下游提供服务
local-mirror -m relay -r 192.168.1.100 -p /path/to/relay
```

启动后打印状态横幅：实际监听端口、同步目录、日志位置：

```
█   █▀█ █▀▀ █▀█ █     █▄ ▄█ ▀█▀ █▀█ █▀█ █▀█ █▀█
█   █ █ █   █▀█ █  ▀▀ █ ▀ █  █  █▀▄ █▀▄ █ █ █▀▄
▀▀▀ ▀▀▀ ▀▀▀ ▀ ▀ ▀▀▀   ▀   ▀ ▀▀▀ ▀ ▀ ▀ ▀ ▀▀▀ ▀ ▀

────────────────────────────────────────────────
  Local Mirror 0.12.0  ·  reality (server)
────────────────────────────────────────────────
  Sync root  /path/to/source
  Ignores    .local-mirror, .git, .DS_Store
  Listen     0.0.0.0:52345
  Encryption on (Noise NNpsk0)
  Instance   3b7b81ee
  PID        62289
  Log        .local-mirror/logs/error.log (level warn)
────────────────────────────────────────────────
```

## 参数

| 参数 | 说明 | 默认值 |
|---|---|---|
| `-m, --mode` | `reality`（服务端）/ `mirror`（客户端）/ `relay`（中继） | `reality` |
| `-p, --path` | 同步根目录，状态目录 `.local-mirror/` 位于其下 | 当前工作目录 |
| `-r, --realityip` | 上游地址（mirror/relay）；留空走局域网发现 | |
| `-a, --alias` | 实例别名，展示在发现列表中 | 主机名 |
| `-i, --ignore` | 追加忽略模式，逗号分隔 | |
| `--config` | 多任务 YAML 配置（与其余参数互斥） | |
| `--allow-delete` | 删除本地多余文件（忠实镜像） | 关 |
| `--allow-critical` | 允许在关键路径上同步，覆盖前备份 | 关 |
| `-c, --cooldown` | 全量扫描间隔（秒），仅客户端 | `1800` |
| `-f, --filebuffersize` | 传输分块大小（字节），仅服务端 | `65536` |
| `-k, --secret` | 传输加密口令（或环境变量 `LOCAL_MIRROR_SECRET`） | |
| `-l, --loglevel` | `debug` / `info` / `warn` / `error` | `error` |

完整说明见 `local-mirror --help`。

忽略模式（来自 `-i` 或 `.local-mirror/ignore` 文件，每行一条，`#` 注释）
按路径段在任意深度匹配，支持 `* ? []` 通配符。服务端命中即不扫描不提供；
客户端命中即不下载也不删除。内置默认只有 `.local-mirror`、`.git`、
`.DS_Store`——想跳过 `node_modules` 之类的请自行添加。

## 删除保护

同步会覆盖已存在的文件，`--allow-delete` 还会删多余的，所以真正要防的
失败模式是把工具指错目录。保护分三级：

1. **默认**——只同步不删除。关键路径直接拒绝启动：家目录、文件系统根，
   以及 `/etc`、`/usr` 等系统目录树连同其子目录。判定前先解引用符号链接。
   家目录内部的普通目录不受限制。
2. **`--allow-critical`**——解锁关键路径上的同步，仍不删除。已存在的文件
   首次被覆盖前，原件先复制到 `.local-mirror/backups/<相对路径>`。
3. **`--allow-delete`**——启用删除。关键路径上必须与 `--allow-critical`
   同时给才生效；普通路径单独给即可。

在这套阶梯之外，服务端下发的每个路径都会校验必须落在同步根之内——恶意或
有 bug 的服务端没办法写到、删到你给定的目录之外。

## 加密

可选，走 Noise 协议（NNpsk0）。两端用 `-k` 配同一个口令即可获得双向认证
和前向保密；口令不对或对端说明文，握手当场失败。只要不是可信局域网，务必
`-k` 加显式 `-r` 一起用——局域网发现是明文的，只设计给可信网络。

## 多任务（YAML）

一台机器要同时共享几个目录、或者边共享边备份别人时，一个 YAML 全部搞定
（示例：[deploy/local-mirror.example.yml](deploy/local-mirror.example.yml)）：

```yaml
defaults:
  loglevel: info

tasks:
  - name: photos          # 任务名 = 发现别名 = 日志前缀
    mode: reality
    path: /srv/photos
    ignore: [cache, "*.log"]
  - name: nas-backup
    mode: mirror
    path: /srv/backup
    realityip: 192.168.1.100
    allow_delete: true
```

```bash
local-mirror --config /etc/local-mirror.yml
```

每个任务是独立子进程：崩溃的任务退避重启，配置错误（退出码 2）只停该
任务，父进程收到 SIGTERM 统一停全部。同机服务端任务共享 52345–52354
端口段，最多 10 个。`secret` 经环境变量传给子进程，不出现在 `ps` 里。

## 服务化运行

Linux 用 systemd——单元文件示例在
[deploy/local-mirror.service](deploy/local-mirror.service)
（deb/rpm 包会把它装到 `/usr/share/doc/local-mirror/examples/`）：

```bash
sudo cp deploy/local-mirror.service /etc/systemd/system/
# 编辑 ExecStart 里的模式/目录/上游地址后：
sudo systemctl daemon-reload
sudo systemctl enable --now local-mirror
journalctl -u local-mirror -f
```

macOS 用 launchd——`brew services` 只支持 formula 不支持 cask，用
LaunchAgent 示例
[deploy/com.xwvike.local-mirror.plist](deploy/com.xwvike.local-mirror.plist)
（发行压缩包里也有）。登录即启动，进程挂了自动拉起：

```bash
cp deploy/com.xwvike.local-mirror.plist ~/Library/LaunchAgents/
# 编辑二进制路径、模式/目录/上游地址与日志路径后：
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.xwvike.local-mirror.plist
# 停止并卸载：
launchctl bootout gui/$(id -u)/com.xwvike.local-mirror
```

两边一样：口令走环境变量注入（unit 里 `Environment=`/`EnvironmentFile=`，
plist 里 `EnvironmentVariables`），别写在 `-k` 参数里，免得出现在 `ps`
输出中。

## 查看监听分级

服务端按活跃度给每个目录打分：热门目录实时监听，冷门目录低频轮询；
事件会给目录加分，沉寂则逐步衰减。想看当前的热度表，给服务端进程发
`SIGUSR1`（仅 Unix），然后读它写出的快照：

```bash
kill -USR1 $(pgrep -f 'local-mirror -m reality')
cat /path/to/source/.local-mirror/heat.txt
```

目录按分数降序列出，带层级和事件计数——可以直观确认你正在干活的
目录有没有拿到实时监听。

## 运行时产物

全部在同步根下的 `.local-mirror/` 里（同步逻辑和 git 都忽略它）：

- `cache.db` — 持久化的目录树；重启时跳过未变化的文件
- `logs/error.log` — 运行日志，单文件 10 MB 轮转，保留最近 3 个
- `partial/` — 中断下载的分片，等待续传
- `backups/` — 覆盖前备份，仅 `--allow-critical` 时产生
- `ignore` — 可选的忽略模式，与 `-i` 合并（改后重启生效）
- `heat.txt` — 监听分级快照，收到 `SIGUSR1` 时写出（仅服务端）

## 开发

日常就是 `go build ./...` 和 `go test ./...`。发版推一个 `v*` tag 即可：
CI 跑 goreleaser，一次产出压缩包、deb/rpm、Homebrew cask 和 Scoop
manifest（本地想全平台构建用 `goreleaser release --snapshot --clean`，
不发布）。

MIT 许可证。
