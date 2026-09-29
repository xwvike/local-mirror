# local-mirror

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/wordmark-dark.svg">
    <img src="assets/wordmark.svg" width="480" alt="LOCAL-MIRROR">
  </picture>
</p>

[English](README.md) | 简体中文

基于 TCP 的单向目录镜像。一端是**发送端**（`--send`），另一端作为**接收端**
（`--receive`），保持它的实时副本。同一进程同时给两个旗子即为中继（A → B → C）。

```
┌─────────────┐   tree / changes / files   ┌─────────────┐
│    source   │ ─────────────────────────▶ │     sink    │
│   --send    │             TCP            │  --receive  │
└─────────────┘                            └─────────────┘
 监听文件变化                               持续拉取，保持同步
```

## 安装

```bash
# Linux（任意发行版）
curl -fsSL https://raw.githubusercontent.com/xwvike/local-mirror/main/install.sh | sh

# macOS
brew install xwvike/tap/local-mirror

# Windows（Scoop；没装 Scoop 先 `irm get.scoop.sh | iex`）
scoop bucket add xwvike https://github.com/xwvike/scoop-bucket
scoop install local-mirror

# 从源码编译
go build -o local-mirror ./cmd/local-mirror
```

## 常用命令

```bash
# ── 1. 局域网：发送端监听，接收端拨号 ─────────────────────────────────────────
# 在 A（发送端）上：--gen-key 生成一把 key（已有则沿用），
# 并打印对端该用的完整命令
local-mirror --send -p /path/to/source --gen-key
# 在 B（接收端）上；192.168.1.100 是 A 的地址
local-mirror --receive --connect 192.168.1.100 -p /path/to/replica -k <printed-key>

# ── 2. 局域网零配置：接收端通过 UDP 自动发现发送端 ────────────────────────────
# 在 A（发送端）上
local-mirror --send -p /path/to/source --gen-key
# 在 B（接收端）上：扫描局域网，选择 A
local-mirror --receive -p /path/to/replica -k <printed-key>

# ── 3. 公网推送：可达的一端（接收端）监听，发送端拨出去 ───────────────────────
# 在 VPS（接收端）上
local-mirror --receive --listen -p /srv/backup --allow-delete --gen-key
# 在家里（发送端）
local-mirror --send --connect vps.example.net:52345 -p /path/to/source -k <printed-key>

# ── 4. 同样的推送，rsync 风格位置写法（./dir @host = 推） ─────────────────────
# 在 VPS（接收端）上
local-mirror --receive --listen -p /srv/backup --allow-delete --gen-key
# 在家里（发送端）
local-mirror -k <printed-key> ./path/to/source @vps.example.net:52345

# ── 5. 中继 A → B → C：B 从 A 拉取，同时供给 C ────────────────────────────────
# 在 A（发送端，192.168.1.100）上
local-mirror --send -p /path/to/source --gen-key
# 在 B（中继，192.168.1.101）上
local-mirror --send --receive --connect 192.168.1.100 -p /path/to/relay -k <printed-key>
# 在 C（接收端）上
local-mirror --receive --connect 192.168.1.101 -p /path/to/replica -k <printed-key>
```

每一端首次运行后都会把 key 存进自己的 `.local-mirror/key`，之后可以省掉 `-k`；
带 `--gen-key` 的命令重复执行会沿用已有的 key。

## flag

| 旗子 | 说明 | 默认 |
|---|---|---|
| `--send` | 本端是发送端：数据流出 | |
| `--receive` | 本端是接收端：数据流入（两个都给 = 中继） | |
| `--connect` | 拨号对端 `host[:port]`；对端须在监听 | |
| `--listen` | 等待对端拨入 | |
| `-p, --path` | 同步根；状态存在其下的 `.local-mirror/` | 工作目录 |
| `-a, --alias` | 发现列表中显示的实例名 | 主机名 |
| `-i, --ignore` | 额外忽略模式，逗号分隔 | |
| `--config` | YAML 配置文件（与其他旗子互斥） | |
| `--allow-delete` | 接收端删除发送端已不存在的多余文件 | 关 |
| `--allow-critical` | 允许在关键路径上同步，覆盖前备份原文件 | 关 |
| `-k, --secret` | 传输加密密钥（或 YAML 里的 `secret:`） | |
| `--gen-key` | 生成随机 key 写入 `.local-mirror/key`（已有则沿用）并打印；带运行 flag 时接着启动 | |
| `--show-key` | 打印已有的 key 文件后退出 | |
| `--no-encrypt` | 即使有 key 文件也强制明文 | |
| `--status` | 打印运行中实例的状态后退出（`--all` 列全部） | |
| `--heat` | 打印运行中发送端的目录热度表后退出 | |
| `-c, --cooldown` | 接收端全量重扫间隔（秒） | `1800` |
| `-f, --filebuffersize` | 发送端传输分块大小（字节） | `65536` |
| `-l, --loglevel` | `debug` / `info` / `warn` / `error` | `error` |


## 方向与传输

两个正交轴：**方向**（`--send` / `--receive`）与**传输**（`--connect` /
`--listen`），自由组合。

- `--connect` 支持域名、IPv4、IPv6 字面量，端口可选（`--connect [2001:db8::1]:52345`）。
- `--receive` 不给 `--connect` 也不给 `--listen` → 局域网发现：UDP 扫描、交互选择发送端（见常用命令第 2 组）。
- 监听的接收端一次只服务一个发送端；会话进行中其他发送端拨入会被拒绝，并按退避重拨。

## 忽略模式

模式来自 `-i` 和/或 `.local-mirror/ignore` 文件（一行一条，`#` 注释）。按路径分段、
任意深度匹配，支持 `* ? []` 通配。

- 发送端命中即不扫描、不供给（目录枚举与直接文件请求都拒绝）；接收端命中即不下载、不删除。
- `.local-mirror`（自身状态目录）永远排除，无法取消忽略。
- `.git`、`.DS_Store` 默认排除但可移除——模式前缀 `!` 即同步（如 `-i '!.git'`）。
  `.git` 复制仓库优先用 git push/fetch，而非文件级镜像。

## 同步什么

- 普通文件（内容与修改时间）和目录，以及两者的权限位（`rwx`）。接收端目录始终保留属主
  `rwx`。与 Windows 之间不同步权限。
- 符号链接、socket、FIFO、设备文件不同步。
- 发送端读不了的内容（文件或整个目录）跳过，接收端副本原样保留，即便开了 `--allow-delete`；
  恢复可读后自动续上。
- 接收端本地被改动的权限，会在其定期本地重扫时按发送端改回。

## 删除保护

同步会覆盖已存在文件；`--allow-delete` 删除多余文件。按同步根分三级：

| 根目录 | 无flag | `--allow-critical` | `--allow-critical` + `--allow-delete` | 仅 `--allow-delete` |
|---|---|---|---|---|
| 普通 | 同步不删 | 同上 | 同步 + 删除 | 同步 + 删除 |
| **关键** | **拒绝** | 同步 + 覆盖前备份 | 同步 + 删除 + 覆盖前备份 | **拒绝** |

关键路径 = home目录、文件系统根、系统树（`/etc`、`/usr` …）含其子目录；先解引用符号
链接再判定。备份落在 `.local-mirror/backups/<相对路径>`，仅首次覆盖前写一次。

## 加密

Noise 协议（NNpsk0）。两端同一个 `-k` 口令 → 双向认证 + 前向保密；口令不符或对端讲
明文，握手失败。

- **监听端绑定所有接口，因此明文监听端拒绝启动。** 任何非回环监听端必须设 key，或显式
  `--no-encrypt` 坚持明文（仅限可信局域网）。拨号端不受影响。
- `--gen-key` 生成强随机 key 写入 `.local-mirror/key`（0600）；在终端里会打印它，
  并给出对端配套的命令。key 文件已存在则沿用，重启时原样重跑同一条命令即可。
  带运行 flag 时同一条命令直接启动。自带口令请用长随机串（`openssl rand -base64 24`）。
- 解析优先级：显式 `-k`（或 YAML 的 `secret:`）＞ `.local-mirror/key` 文件 ＞ 明文。
  用 `-k` 启动的一端会把 key 存进自己的 key 文件（接收端，以及拨出的发送端），之后可省 `-k`。
- `--show-key` 打印文件；`--gen-key --force` 重新生成。在监听端重新生成会断开所有已连
  拨号端。

## 观察：`--status` / `--heat`

只读、按需，针对运行中的实例（`-p` 指向其同步根，或在根内运行）。只有在有人观察时，
常驻进程才写 `status.json` / `heat.json`。

```bash
local-mirror --status -p /path/to/source     # 加 --all 列出所有进程
local-mirror --heat   -p /path/to/source     # 仅发送端；接收端没有热度表
```

`--all` 从进程表找出本机所有实例；macOS 上只能找到以绝对路径启动的实例。

```
──────────────────────────────────────────────────────
  Status      ● running   pid 62289 · up 3h12m
  Direction   send · source   (listen)
  Link        ● serving 192.168.1.50:54012
  Encryption  on (Noise NNpsk0)
  Transfer    ▶ docs/report.pdf  ██████████░░░░░░  4.2 MB / 8.1 MB  5.3 MB/s
  Totals      1.2 GB / 3841 files    Errors 0
──────────────────────────────────────────────────────
```

`--heat`：发送端按活跃度给每个目录打分，热的实时监听（tier1）、冷的懒轮询（tier2）。表按
热度从高到低列出——用来确认活跃目录拿到了实时监听。

## 多任务（YAML）

一个文件跑多个目录（示例：[deploy/local-mirror.example.yml](deploy/local-mirror.example.yml)）：

```yaml
defaults:
  loglevel: info
  secret: <key>           # 监听的任务必须有 key（或在其同步根放 .local-mirror/key）
tasks:
  - name: photos          # 任务名 = 发现别名 = 日志前缀
    send: true
    path: /srv/photos
    ignore: [cache, "*.log"]
  - name: nas-backup
    receive: true
    connect: 192.168.1.100
    path: /srv/backup
    allow_delete: true
```

```bash
local-mirror --config /etc/local-mirror.yml
```

每个任务用与命令行相同的方向键（`send` / `receive` / `connect` / `listen`；`send`
和 `receive` 都给 = 中继）。单任务在本进程内跑；两个及以上每任务各起一个子进程（崩溃
→ 退避重启；配置错误 = exit 2，只停该任务；父进程收 SIGTERM 全停）。服务端任务共用
52345–52354 端口（最多十个）。`secret` 经 stdin 送给子进程——`ps` 和环境变量里都看不到。
**配置文件不得位于任何任务的同步根内部**（会连同 secret 一起被镜像出去），local-mirror
会拒绝加载这样的配置。

## 作为服务运行

```bash
local-mirror service install     # Linux 默认 --system，macOS 默认 --user
# 会打印配置路径；填好任务，然后：
sudo systemctl enable --now local-mirror                    # Linux（systemd）
launchctl kickstart -k gui/$(id -u)/com.xwvike.local-mirror # macOS（launchd）

local-mirror service status      # 配置与服务在哪、是否已注册
local-mirror service uninstall   # 注销并删除服务文件；配置保留
local-mirror service install --dry-run   # 只打印将写入/执行的内容，不动任何东西
```

- 配置路径：`/etc/local-mirror/config.yml`（系统级）或
  `~/.config/local-mirror/config.yml`（用户级）。
- 自动识别 init 系统：**systemd**、**launchd**、**procd**（OpenWrt →
  `/etc/init.d/local-mirror`，输出进 `logread`）。
- Linux 上服务以调用者身份运行（非 root，否则同步来的文件会归 root）；`--run-as <user>`
  指定其他用户。口令放配置的 `secret:`（0600）或 `.local-mirror/key` 文件，别用 `-k`
  （`ps` 里可见）。

## `.local-mirror/` 下的文件

都在同步根内，且不被同步、不进 git：

| 文件 | 用途 |
|---|---|
| `cache.db` | 持久化目录树；重启时跳过未变文件 |
| `key` | 自管理传输 key（0600），省略 `-k` 时自动加载 |
| `status.json` | 实时状态，仅 `--status` 观察时写；可丢 |
| `heat.json` | 热度表，仅 `--heat` 观察时写（发送端）；可丢 |
| `logs/error.log` | 运行日志，10 MB 轮转，保留最近 3 份 |
| `partial/` | 中断下载的分块，等待续传 |
| `backups/` | 覆盖前副本，仅 `--allow-critical` 时产生 |
| `ignore` | 可选忽略模式，与 `-i` 合并（重启生效） |

## 开发

```bash
go build ./...
go test ./...
```

推 `v*` tag 触发发版：CI 跑 goreleaser，一次性发布归档、Homebrew cask 与 Scoop 清单
（`goreleaser release --snapshot --clean` 本地全量构建但不发布）。

MIT 许可。
