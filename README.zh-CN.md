# local-mirror

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/wordmark-dark.svg">
    <img src="assets/wordmark.svg" width="480" alt="LOCAL-MIRROR">
  </picture>
</p>

[English](README.md) | 简体中文

基于 TCP 的单向目录镜像。一端是**源**（`--send`），另一端保持它的实时副本、作为
**汇**（`--receive`）。同一进程同时给两个旗子即为中继（A → B → C）。

```
┌─────────────┐    目录树 / 变更 / 文件    ┌─────────────┐
│    源 source │ ─────────────────────────▶ │   汇 sink   │
│    --send    │           TCP           │  --receive  │
└─────────────┘                            └─────────────┘
  监听文件变化                                持续拉取，保持一致
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
# 分享一个目录（-p 缺省为当前工作目录）
local-mirror --send -p /path/to/source

# 在另一台机器上复制它，拨号已知主机
local-mirror --receive --connect 192.168.1.100 -p /path/to/replica

# 局域网零配置复制：不给 --connect/--listen → 扫描源并交互选择
local-mirror --receive -p /path/to/replica

# 中继：一个进程同时从上游拉、向下游供
local-mirror --send --receive --connect 192.168.1.100 -p /path/to/relay

# 公网推送：可达的一端作汇并监听，源拨出去。
# 监听端必须设 key（见「加密」）。
local-mirror --receive --listen -p /srv/backup --allow-delete --gen-key   # 打印一把 key
local-mirror --send --connect a.example.net:52345 -p /path/to/source -k <printed-key>

# 同样的推送，rsync 风格位置写法（./dir @host = 推）
local-mirror ./path/to/source @a.example.net:52345 -k <printed-key>
```

## flag

| 旗子 | 说明 | 默认 |
|---|---|---|
| `--send` | 本端是源：数据流出 | |
| `--receive` | 本端是汇：数据流入（两个都给 = 中继） | |
| `--connect` | 拨号对端 `host[:port]`；对端须在监听 | |
| `--listen` | 等待对端拨入 | |
| `-p, --path` | 同步根；状态存在其下的 `.local-mirror/` | 工作目录 |
| `-a, --alias` | 发现列表中显示的实例名 | 主机名 |
| `-i, --ignore` | 额外忽略模式，逗号分隔 | |
| `--config` | YAML 配置文件（与其他旗子互斥） | |
| `--allow-delete` | 删除汇端上游已不存在的多余文件 | 关 |
| `--allow-critical` | 允许在关键路径上同步，覆盖前备份原文件 | 关 |
| `-k, --secret` | 传输加密密钥（或 YAML 里的 `secret:`） | |
| `--gen-key` | 生成随机 key 写入 `.local-mirror/key`，打印后退出 | |
| `--show-key` | 打印已有的 key 文件后退出 | |
| `--no-encrypt` | 即使有 key 文件也强制明文 | |
| `--status` | 打印运行中实例的状态后退出（`--all` 列全部） | |
| `--heat` | 打印运行中源的目录热度表后退出 | |
| `-c, --cooldown` | 汇端全量重扫间隔（秒） | `1800` |
| `-f, --filebuffersize` | 源端传输分块大小（字节） | `65536` |
| `-l, --loglevel` | `debug` / `info` / `warn` / `error` | `error` |


## 方向与传输

两个正交轴：**方向**（`--send` / `--receive`）与**传输**（`--connect` /
`--listen`），自由组合。

- `--connect` 支持域名、IPv4、IPv6 字面量（`local-mirror --connect [2001:db8::1]:52345`）；
- `--receive` 局域网发现：UDP 扫描、交互选源（`local-mirror --receive`）。
## 忽略模式

模式来自 `-i` 和/或 `.local-mirror/ignore` 文件（一行一条，`#` 注释）。按路径分段、
任意深度匹配，支持 `* ? []` 通配。

- 源端命中即不扫描、不供给（目录枚举与直接文件请求都拒绝）；汇端命中即不下载、不删除。
- `.local-mirror`（自身状态目录）永远排除，无法取消忽略。
- `.git`、`.DS_Store` 默认排除但可移除——模式前缀 `!` 即同步（如 `-i '!.git'`）。
  `.git` 复制仓库优先用 git push/fetch，而非文件级镜像。

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
- `--gen-key` 生成强随机 key 写入 `.local-mirror/key`（0600），打印一次，并（带运行
  flag时）同一条命令直接启动。自带口令请用长随机串（`openssl rand -base64 24`）。
- 解析优先级：显式 `-k`（或 YAML 的 `secret:`）＞ `.local-mirror/key` 文件 ＞ 明文。
  拨号端首次连上后会自存一份副本。
- `--show-key` 打印文件；`--gen-key --force` 重新生成。在监听端重新生成会断开所有已连
  拨号端。

## 观察：`--status` / `--heat`

只读、按需，针对运行中的实例（`-p` 指向其同步根，或在根内运行）。只有在有人观察时，
常驻进程才写 `status.json` / `heat.json`。

```bash
local-mirror --status -p /path/to/source     # 加 --all 列出所有进程
local-mirror --heat   -p /path/to/source     # 仅源端；汇端没有热度表
```

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

`--heat`：源按活跃度给每个目录打分，热的实时监听（tier1）、冷的懒轮询（tier2）。表按
热度从高到低列出——用来确认活跃目录拿到了实时监听。

## 多任务（YAML）

一个文件跑多个目录（示例：[deploy/local-mirror.example.yml](deploy/local-mirror.example.yml)）：

```yaml
defaults:
  loglevel: info
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
| `heat.json` | 热度表，仅 `--heat` 观察时写（源端）；可丢 |
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
