# local-mirror

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/wordmark-dark.svg">
    <img src="assets/wordmark.svg" width="480" alt="LOCAL-MIRROR">
  </picture>
</p>

[English](README.md) | 简体中文

基于 TCP 的单向目录镜像。一端为**发送端**（`--send`），另一端为**接收端**
（`--receive`），保存其实时副本。同一进程同时指定两者即为中继（A → B → C）。

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

# Windows（Scoop；未安装 Scoop 时先执行 `irm get.scoop.sh | iex`）
scoop bucket add xwvike https://github.com/xwvike/scoop-bucket
scoop install local-mirror

# 从源码编译
go build -o local-mirror ./cmd/local-mirror
```

## 常用命令

以下命令成对在前台运行。验证无误后，每一组均可用相同参数安装为服务，见下节「作为服务运行」。

```bash
# ── 1. 局域网：发送端监听，接收端拨号 ─────────────────────────────────────────
# A（发送端）。--gen-key 生成 key（已有则沿用），并打印 B 所需的命令。
local-mirror --send -p /path/to/source --gen-key
# B（接收端）。192.168.1.100 为 A 的地址。
local-mirror --receive --connect 192.168.1.100 -p /path/to/replica -k <printed-key>

# ── 2. 局域网零配置：接收端通过 UDP 自动发现发送端 ────────────────────────────
# A（发送端）。
local-mirror --send -p /path/to/source --gen-key
# B（接收端）。扫描局域网中的发送端并提示选择。
local-mirror --receive -p /path/to/replica -k <printed-key>

# ── 3. 公网推送：可达的接收端监听，发送端主动连接 ─────────────────────────────
# VPS（接收端）。
local-mirror --receive --listen -p /srv/backup --allow-delete --gen-key
# 客户端（发送端）。
local-mirror --send --connect vps.example.net:52345 -p /path/to/source -k <printed-key>

# ── 4. 公网推送的位置参数写法（./dir @host 表示推送） ─────────────────────────
# VPS（接收端）。
local-mirror --receive --listen -p /srv/backup --allow-delete --gen-key
# 客户端（发送端）。
local-mirror -k <printed-key> ./path/to/source @vps.example.net:52345

# ── 5. 中继 A → B → C：B 从 A 接收，同时向 C 提供 ─────────────────────────────
# A（发送端，192.168.1.100）。
local-mirror --send -p /path/to/source --gen-key
# B（中继，192.168.1.101）。
local-mirror --send --receive --connect 192.168.1.100 -p /path/to/relay -k <printed-key>
# C（接收端）。
local-mirror --receive --connect 192.168.1.101 -p /path/to/replica -k <printed-key>
```

每一端首次运行时将 key 保存到各自的 `.local-mirror/key`，此后可省略 `-k`。
再次执行带 `--gen-key` 的命令会沿用已有的 key。

## 作为服务运行（推荐）

local-mirror 设计为在后台持续运行，使副本保持实时；一次性拷贝更适合使用 rsync。
「常用命令」中的一组命令先在前台验证，再以相同参数安装为服务：Linux 上为
`sudo local-mirror service install <参数>`，macOS 上为 `local-mirror service install <参数>`
（用户级服务，无需 `sudo`）。服务随系统启动，异常退出后自动重启。

示例：Linux 服务器为接收端，Mac 为发送端。

```bash
# ── 服务器：Linux，接收端，地址为 vps.example.net ─────────────────────────────
# 前台运行。--gen-key 生成 key，并打印发送端所需的命令。
local-mirror --receive --listen -p /srv/backup --allow-delete --gen-key
# 以 Ctrl+C 停止后安装为服务，沿用已有的 key。
sudo local-mirror service install --receive --listen -p /srv/backup --allow-delete --gen-key

# ── 客户端：macOS，发送端 ─────────────────────────────────────────────────────
# 前台运行。
local-mirror --send --connect vps.example.net:52345 -p ~/projects -k <printed-key>
# 以 Ctrl+C 停止后安装为服务。
local-mirror service install --send --connect vps.example.net:52345 -p ~/projects -k <printed-key>
```

Linux 服务器上的服务管理命令如下。macOS 上省略 `sudo`，配置文件为
`~/.config/local-mirror/config.yml`，日志为 `~/Library/Logs/local-mirror.log`。

```bash
# 显示配置文件、任务、服务状态及管理命令。
local-mirror service status
# 以新参数重新安装某个目录即修改其参数（此处关闭删除）。
sudo local-mirror service install --receive --listen -p /srv/backup --gen-key
# 使手工编辑的配置生效。配置有错时拒绝执行，正在运行的服务不受影响。
sudoedit /etc/local-mirror/config.yml
sudo local-mirror service restart
# 删除一个目录。删除最后一个目录时停止并卸载服务。
sudo local-mirror service remove -p /srv/backup
# 停止并卸载服务，保留配置文件。
sudo local-mirror service uninstall
# 同步状态与服务日志。
local-mirror --status --all
sudo journalctl -u local-mirror
```

安装其他目录时，任务加入同一服务。

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
- `--receive` 未指定 `--connect` 与 `--listen` 时进行局域网发现：通过 UDP 扫描并交互选择发送端（见常用命令第 2 组）。
- 监听的接收端同一时间只服务一个发送端；会话期间其他发送端的连接被拒绝，并按退避间隔重试。

## 忽略模式

模式来自 `-i` 和/或 `.local-mirror/ignore` 文件（一行一条，`#` 注释）。按路径分段、
任意深度匹配，支持 `* ? []` 通配。

- 发送端命中即不扫描、不供给（目录枚举与直接文件请求都拒绝）；接收端命中即不下载、不删除。
- `.local-mirror`（自身状态目录）永远排除，无法取消忽略。
- `.git`、`.DS_Store` 默认排除，加 `!` 前缀可重新纳入同步，例如
  `local-mirror --send -p <目录> --gen-key -i '!.git'`。`.git` 仓库的复制宜使用
  git push/fetch，而非文件级镜像。

## 同步什么

- 普通文件（内容与修改时间）和目录，以及两者的权限位（`rwx`）。接收端目录始终保留属主
  `rwx`。与 Windows 之间不同步权限。
- 符号链接、socket、FIFO、设备文件不同步。
- 发送端无法读取的内容（文件或整个目录）被跳过，接收端副本保持不变，即使启用了
  `--allow-delete`；恢复可读后自动继续同步。
- 接收端本地被修改的权限，在其定期本地重扫时恢复为发送端的权限。

## 首次同步

尚未完成首次同步的接收端，由发送端把整棵树连续推送过来，不再逐个文件、逐个目录发请求。
在远距离链路上，小文件很多时首次同步的大部分耗时由此省去。

- 目标目录里已有的文件不影响首次同步。与发送端同路径的文件被覆盖；发送端没有的文件保留，
  启用 `--allow-delete` 时删除。
- 中断的首次同步在下次连接时从最后记录的位置继续，接收了一部分的文件从断点续传。
- 进度记录在 `.local-mirror` 中。删除该目录或更换同步根，会重新进行一次整棵树的首次同步。
- 对端不支持时，首次同步逐个文件进行。

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
- `--gen-key` 生成随机 key 并写入 `.local-mirror/key`（0600）；在终端中同时打印 key
  及对端所需的命令。key 文件已存在时沿用，因此重启后可原样再次执行同一命令。与运行
  参数同时使用时，同一命令直接启动实例。自定义 key 应使用足够长的随机串
  （`openssl rand -base64 24`）。
- 解析优先级：显式 `-k`（或 YAML 的 `secret:`）＞ `.local-mirror/key` 文件 ＞ 明文。
  用 `-k` 启动的一端会把 key 存进自己的 key 文件（接收端，以及拨出的发送端），之后可省 `-k`。
- `local-mirror --show-key -p <目录>` 打印 key（仅限终端）；
  `local-mirror --gen-key --force -p <目录>` 重新生成 key。在监听端重新生成会断开所有
  已连接的拨号端。

## 观察：`--status` / `--heat`

在终端中前台运行时，启动横幅下方显示链接状态、当前传输、本次会话的传输量、错误数和
最近几条日志，每秒刷新；此期间日志只写入日志文件。输出被重定向或作为服务运行时不显示。

只读命令，作用于运行中的实例，由 `-p`（实例的同步根）或当前目录指定。仅在这些命令
运行期间，常驻进程才写入 `status.json` / `heat.json`。

```bash
# 显示同步根为 /path/to/source 的实例状态。
local-mirror --status -p /path/to/source
# 显示本机所有实例的状态。
local-mirror --status --all
# 显示发送端的目录热度表（接收端没有热度表）。
local-mirror --heat -p /path/to/source
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

`--heat`：发送端按活跃度为每个目录评分，热目录实时监听（tier1），冷目录延迟轮询
（tier2）。表按热度从高到低排列，可用于确认活跃目录是否处于实时监听。

## 服务细节

`local-mirror service install` 把任务保存在一份 YAML 配置里：系统级服务（Linux 默认）是
`/etc/local-mirror/config.yml`，用户级服务（macOS 默认）是 `~/.config/local-mirror/config.yml`。
该文件也可手工编辑，编辑后执行 `sudo local-mirror service restart`（Linux）或
`local-mirror service restart`（macOS）使其生效。示例：
[deploy/local-mirror.example.yml](deploy/local-mirror.example.yml)。

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

- 每个任务用与命令行相同的方向键（`send` / `receive` / `connect` / `listen`；`send`
  和 `receive` 都给 = 中继）。单任务在本进程内运行；两个及以上时每个任务各启动一个子进程
  （崩溃 → 退避重启；配置错误 = exit 2，只停该任务）。监听的任务共用 52345–52354
  端口（最多十个）。
- key 保存在配置的 `secret:` 中（0600，仅服务运行用户可读），不出现在命令行中（命令行
  对 `ps` 可见）。**配置文件不得位于任何任务的同步根内部**（会连同 secret 一起被镜像出去），
  local-mirror 会拒绝加载这样的配置。
- Linux 上服务以执行 `sudo` 的用户身份运行，而非 root，同步的文件因此不归 root 所有。
  指定其他用户：`sudo local-mirror service install --run-as <用户> <参数>`。
- 服务级别默认跟随平台（Linux 系统级，macOS 用户级），每条服务命令都可以用 `--system`
  或 `--user` 改变它。
- 自动识别 init 系统：**systemd**、**launchd**、**procd**（OpenWrt：`/etc/init.d/local-mirror`，
  输出进 `logread`）。
- `local-mirror service install --dry-run <参数>` 打印将写入的文件与将执行的命令，不修改
  系统。
- 同一份配置也可以在前台运行：`local-mirror --config <file>`。

## `.local-mirror/` 下的文件

都在同步根内，且不被同步、不进 git：

| 文件 | 用途 |
|---|---|
| `cache.db` | 持久化目录树；重启时跳过未变文件 |
| `key` | 自管理传输 key（0600），省略 `-k` 时自动加载 |
| `status.json` | 实时状态，仅 `--status` 观察时写；可删除 |
| `heat.json` | 热度表，仅 `--heat` 观察时写（发送端）；可删除 |
| `logs/error.log` | 运行日志，10 MB 轮转，保留最近 3 份 |
| `partial/` | 中断下载的分块，等待续传 |
| `backups/` | 覆盖前副本，仅 `--allow-critical` 时产生 |
| `ignore` | 可选忽略模式，与 `-i` 合并（重启生效） |

## 开发

```bash
go build ./...
go test ./...
```

推 `v*` tag 触发发版：CI 运行 goreleaser，一次性发布归档、Homebrew cask 与 Scoop 清单
（`goreleaser release --snapshot --clean` 本地全量构建但不发布）。

MIT 许可。
