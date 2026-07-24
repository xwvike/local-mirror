# local-mirror

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/wordmark-dark.svg">
    <img src="assets/wordmark.svg" width="480" alt="LOCAL-MIRROR">
  </picture>
</p>

[English](README.md) | 简体中文

基于 TCP 的单向目录镜像。一端是**源**（`--send`），另一端保持它的实时副本、
作为**汇**（`--receive`），也可以经中继级联成 A → B → C 传递链。

```
┌─────────────┐    目录树 / 变更 / 文件    ┌─────────────┐
│    源 source │ ─────────────────────────▶ │   汇 sink   │
│    --send    │           TCP           │  --receive  │
└─────────────┘                            └─────────────┘
  监听文件变化                                持续拉取，保持一致
```

数据方向与传输方向相互独立：**拨号方和监听方可以是任意一端**。

接受端（汇端）默认只做增量：删除必须在启动时显式加参数。
文件按 blake3 哈希比对，分块传输、原子落盘，支持断点续传。变更通常
两秒左右到达接受端，默认每 30 分钟一次的全量扫描兜底丢失的通知。符号链接
不同步也不解引用。

## 安装

macOS：

```bash
brew install xwvike/tap/local-mirror
```

Windows，使用 [Scoop](https://scoop.sh)（没装 Scoop 的话，执行 `irm get.scoop.sh | iex`
安装）：

```powershell
scoop bucket add xwvike https://github.com/xwvike/scoop-bucket
scoop install local-mirror
```

Linux（任意发行版；macOS 不想用 Homebrew 也能用）：

```bash
curl -fsSL https://raw.githubusercontent.com/xwvike/local-mirror/main/install.sh | sh
```

[releases 页面](https://github.com/xwvike/local-mirror/releases)


或从源码构建：

```bash
git clone https://github.com/xwvike/local-mirror && cd local-mirror
go build -o local-mirror ./cmd/local-mirror
```

## 快速开始

```bash
# 源：共享指定目录（-p 省略时为当前工作目录）
local-mirror --send -p /path/to/source

# 另一台机器上镜像它
local-mirror --receive --connect 192.168.1.100 -p /path/to/replica

# 不指定 --connect：自动发现局域网的源并交互选择
local-mirror --receive -p /path/to/replica

# 中继：从上游接收下来，同时向下游提供服务
local-mirror --send --receive --connect 192.168.1.100 -p /path/to/relay
```

支持反转监听，即有公网IP的服务器充当接受端（汇端），发送端（源端）主动连接：

```bash
# 设备A（有公网IP）
local-mirror --receive --listen -p /srv/backup --allow-delete
# 设备B（无公网IP）
local-mirror --send --connect a.example.net:52345 -p /path/to/source

# 同样效果，rsync 风格的位置参数糖（./dir @host = 推送）
local-mirror ./path/to/source @vps.example.net:52345
```

启动后打印状态横幅：实际监听端口、同步目录、日志位置：

```
█   █▀█ █▀▀ █▀█ █     █▄ ▄█ ▀█▀ █▀█ █▀█ █▀█ █▀█
█   █ █ █   █▀█ █  ▀▀ █ ▀ █  █  █▀▄ █▀▄ █ █ █▀▄
▀▀▀ ▀▀▀ ▀▀▀ ▀ ▀ ▀▀▀   ▀   ▀ ▀▀▀ ▀ ▀ ▀ ▀ ▀▀▀ ▀ ▀

────────────────────────────────────────────────
  Local Mirror 2.0.0  ·  reality (server)
────────────────────────────────────────────────
  Sync root  /path/to/source
  Ignores    .local-mirror, .git, .DS_Store
  Listen     :52345 (IPv4 + IPv6)
  Encryption on (Noise NNpsk0, fp 3f9a…c71e)
  Instance   3b7b81ee
  PID        62289
  Log        .local-mirror/logs/error.log (level warn)
────────────────────────────────────────────────
```


## 参数

| 参数 | 说明 | 默认值 |
|---|---|---|
| `--send` | 本端是源：数据流出 | |
| `--receive` | 本端是汇：数据流入（两个都给 = 中继） | |
| `--connect` | 拨向 `host[:port]`；对端须在监听 | |
| `--listen` | 等对端拨进来 | |
| `-p, --path` | 同步工作目录，状态目录 `.local-mirror/` 位于其下 | 当前工作目录 |
| `-a, --alias` | 实例别名，展示在发现列表中 | 主机名 |
| `-i, --ignore` | 追加忽略模式，逗号分隔 | |
| `--config` | 多任务 YAML 配置（与其余参数互斥） | |
| `--allow-delete` | 允许在同步中删除汇端工作目录里的多余文件（忠实镜像） | 关 |
| `--allow-critical` | 允许在关键路径上同步，覆盖前备份 | 关 |
| `-k, --secret` | 设置传输预加密密钥（或通过环境变量 `LOCAL_MIRROR_SECRET` 设置） | |
| `--gen-key` | 生成随机密钥写入 `.local-mirror/key`，打印后退出 | |
| `--show-key` | 打印工作目录中已有的密钥文件 | |
| `--no-encrypt` | 即使工作目录存在密钥文件也强制明文 | |
| `--status` | 打印运行中实例的状态后退出（`--all` 看全部） | |
| `--heat` | 打印运行中源端的目录热度表后退出 | |
| `-c, --cooldown` | 全量扫描间隔（秒），仅汇端 | `1800` |
| `-f, --filebuffersize` | 传输分块大小（字节），仅源端 | `65536` |
| `-l, --loglevel` | `debug` / `info` / `warn` / `error` | `error` |

完整说明见 `local-mirror --help`。

### 方向与传输

两个相互独立的轴：**方向**（`--send` / `--receive`）和**传输**
（`--connect` / `--listen`）。支持随意组合：可达的服务器上 `--receive --listen`，NAT
后面 `--send --connect`。`--connect` 接受域名、IPv4，或 IPv6 字面量
（`--connect [2001:db8::1]:52345`）；监听方同时绑 IPv4 与 IPv6。使用域名每次重连
都重新解析，DDNS 天然可用。`--send` 和 `--receive` 都给即为中继。

### 局域网发现

`--receive` 且既不带 `--connect` 也不带 `--listen` 时，会通过 UDP 在本地网络
扫描源端,多个应答时交互选择。这是同一局域网内两台机器的零配置路径——不用
填地址、不用记端口。仅这一种情况会触发:给了 `--connect` 就直连已知主机,给了
`--listen` 就等对端拨入。发现只在本网段进行,不跨 VPN、子网或防火墙——那些场景
请改用 `--connect <host>`。设了密钥时,探测与应答都带认证,没有密钥的扫描者
什么也拿不到。

忽略模式（来自 `-i` 或 `.local-mirror/ignore` 文件，每行一条，`#` 注释）
按路径段在任意深度匹配，支持 `* ? []` 通配符。服务端命中即不扫描不提供；
客户端命中即不下载也不删除。`.local-mirror`（工具自己的状态目录）强制排除、
不可取消；`.git` 与 `.DS_Store` 默认排除但可取消——模式前加 `!` 即让它参与
同步（如 `-i '!.git'`）。注意 `.git` 是活的数据库，仓库该用 git 自己复制
（push/fetch）而非文件镜像。`node_modules` 之类的请自行添加。

## 删除保护

同步会覆盖已存在的文件，使用 `--allow-delete` 参数还会删多余的，
所以为了防止工具错误设置工作目录。保护分三级：

1. **默认**——只同步不删除。关键路径拒绝启动：home目录、文件系统根，
   以及 `/etc`、`/usr` 等系统目录树连同其子目录。判定前先解引用符号链接。
   home目录内部的普通目录不受限制。
2. **`--allow-critical`**——解锁关键路径上的同步，仍不删除。已存在的文件
   首次被覆盖前，原件先复制备份到 `.local-mirror/backups/<相对路径>`。
3. **`--allow-delete`**——启用删除。关键路径上必须与 `--allow-critical`
   同时给才生效；普通路径单独给即可。


## 加密

可选，走 Noise 协议（NNpsk0）。两端用 `-k` 配同一个口令即可获得双向认证
和前向保密；口令不对或对端设置明文，握手失败。非可信局域网，建议`-k` 显式指定。

`-k` 建议长随机串（比如 `openssl rand -base64 24`）

`--gen-key` 会把强随机密钥写入
`.local-mirror/key`（权限 600），打印一次后退出；添加运行参数会生成后自动
启动。省略 `-k` 时会自动加载密钥文件，所以监听端生成后、拨号端使用 `-k` 在首次启动设置一次
即可（拨号端随后自存一份）：

```bash
# 监听端
local-mirror --gen-key --send            # 打印密钥后开始服务
# 拨号端，仅首次连接
local-mirror --receive --connect vps.example.net -k <生成的密钥>
```

解析优先级为：显式 `-k`（含环境变量）＞ `.local-mirror/key` 文件 ＞ 明文。
`--show-key` 打印已有文件，`--no-encrypt` 即使有文件也强制明文，
`--gen-key --force` 重新生成。监听端有拨号方连着时请勿删密钥文件——重新生成密钥
会把每一个拨号方都踢下线。

## 查看运行中的实例

`--status` 是独立的只读命令,按需工作:有 `--status` 在看时,常驻进程每秒把
状态写到 `.local-mirror/status.json`、命令负责渲染;没人看时常驻进程根本不写,
所以不观测就零开销。可以使用 `-p` 指向一个同步工作目录（或直接在目录中运行）。

```bash
local-mirror --status -p /path/to/source
```

```
──────────────────────────────────────────────────────
  Status      ● running   pid 62289 · up 3h12m
──────────────────────────────────────────────────────
  Direction   send · source   (listen)
  Peer        inbound
  Link        ● serving 192.168.1.50:54012
  Encryption  on (Noise NNpsk0)
  Sync root   /path/to/source
──────────────────────────────────────────────────────
  Transfer    ▶ docs/report.pdf
              ██████████░░░░░░░░░░  4.2 MB / 8.1 MB   5.3 MB/s
  Totals      1.2 GB / 3841 files   · last 2s ago (docs/report.pdf)
  Errors      0
──────────────────────────────────────────────────────
  CPU         1.4%
  Memory      31 MB rss   (8.1 MB heap · 22 MB sys)
  FDs         14
  Goroutines  27
──────────────────────────────────────────────────────
```

`--all`——会从进程表里找出运行中
的 `local-mirror` 进程，每个打印一行（`--config` 部署也是同一张表，按任务名
索引）：

```bash
local-mirror --status --all
```

```
  NAME             DIR    LINK  RATE        FILES   LAST      CPU    MEM
  proj/src         send   ●     5.3 MB/s    3841    2s        1.4%   31 MB
  srv/backup       recv   ●     —           912     1m        0.2%   18 MB
  media/photos     send   ○     —           220     14m       0.0%   12 MB
```

## 多任务（YAML）

单台要同时共享几个目录、或者边共享，边备份别人时，使用 YAML 可以方便地管理多个任务。
（示例：[deploy/local-mirror.example.yml](deploy/local-mirror.example.yml)）：

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

每个任务用和命令行一样的 `--send` / `--receive` / `--connect` / `--listen`
方向:`send: true` 提供服务,`receive:` 拉取镜像,`connect:` 拨向主机,
`listen: true` 等对端拨入,`send` 与 `receive` 都给即中继。

每个任务是独立子进程：崩溃的任务退避重启，配置错误（退出码 2）只停该
任务，父进程收到 SIGTERM 统一停全部。同机服务端任务共享 52345–52354
端口段，最多 10 个。`secret` 经环境变量传给子进程，不出现在 `ps` 里。

## 服务化运行

Linux 用 systemd——单元文件示例在
[deploy/local-mirror.service](deploy/local-mirror.service)
（deb/rpm 包会把它装到 `/usr/share/doc/local-mirror/examples/`）：

```bash
sudo cp deploy/local-mirror.service /etc/systemd/system/
# 编辑 ExecStart 里的方向/目录/上游地址后：
sudo systemctl daemon-reload
sudo systemctl enable --now local-mirror
journalctl -u local-mirror -f
```

macOS 用 launchd——`brew services` 只支持 formula 不支持 cask，用
LaunchAgent 示例
[deploy/com.xwvike.local-mirror.plist](deploy/com.xwvike.local-mirror.plist)
（发行压缩包里也有）。登录即启动，自动拉起：

```bash
cp deploy/com.xwvike.local-mirror.plist ~/Library/LaunchAgents/
# 编辑二进制路径、方向/目录/上游地址与日志路径后：
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.xwvike.local-mirror.plist
# 停止并卸载：
launchctl bootout gui/$(id -u)/com.xwvike.local-mirror
```

两边一样：口令走环境变量注入（unit 里 `Environment=`/`EnvironmentFile=`，
plist 里 `EnvironmentVariables`），不推荐在 `-k` 参数里，免得出现在 `ps`
输出中。

## 查看监听分级

源端按活跃度给每个目录打分：热门目录实时监听，冷门目录低频轮询；事件会给
目录加分，沉寂则逐步衰减。用 `--heat` 读取（独立的只读命令，和 `--status`
一个套路）；源端只在你观测时才把这张表写到 `.local-mirror/heat.json`：

```bash
local-mirror --heat -p /path/to/source   # 或 --heat --all 看全机所有源
```

```
  heat   /path/to/source
  tier1 (real-time watch) 3/512 · tier2 (lazy poll) 40 · 43 dirs
  SCORE     TIER   EVENTS   DIRECTORY
  128.45    tier1  3410     assets/img
   42.10    tier1  890      src
    3.20    tier2  12       docs
```

目录按分数降序列出，带层级和事件计数——可以直观确认你正在干活的目录有没有
拿到实时监听。汇端不会构建这张表。

## 运行时产物

全部在工作目录下的 `.local-mirror/` 里（同步逻辑和 git 都忽略它）：

- `cache.db` — 持久化的目录树；重启时跳过未变化的文件
- `key` — 自管理的传输密钥（权限 600），省略 `-k` 时自动加载，从不同步
  （`--gen-key` 写入，`--show-key` 打印）
- `status.json` — 实时状态，仅在 `--status` 观测时才写；可弃
- `heat.json` — 目录热度表，仅在 `--heat` 观测时才写（仅源端）；可弃
- `logs/error.log` — 运行日志，单文件 10 MB 轮转，保留最近 3 个
- `partial/` — 中断下载的分片，等待续传
- `backups/` — 覆盖前备份，仅 `--allow-critical` 时产生
- `ignore` — 可选的忽略模式，与 `-i` 合并（改后重启生效）
