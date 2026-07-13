# local-mirror

[English](README.md) | 简体中文

基于 TCP 的单向目录镜像。一台机器共享目录（`reality` 模式），其他机器保持
它的实时副本（`mirror`），也可以经中继级联成 A → B → C 传递链。单个静态
二进制加一套小型自定义二进制协议——不依赖外部服务，不需要云端，除了这个
二进制什么都不用装。

```
┌─────────────┐    目录树 / 变更 / 文件    ┌─────────────┐
│   reality   │ ─────────────────────────▶ │   mirror    │
│  （服务端）  │       自定义 TCP 协议      │  （客户端）  │
└─────────────┘                            └─────────────┘
  监听文件变化                                持续拉取，保持一致
```

镜像默认只做增量：客户端本地多出来的文件不会被动。删除必须显式加
`--allow-delete` 才启用；即使加了，同步根落在关键路径（家目录、`/etc`、
系统目录树）上时也会拒绝启动，除非再加 `--allow-critical`。

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

启动后打印状态横幅（实际监听端口、同步目录、日志位置）：

```
────────────────────────────────────────────────
  Local Mirror v1.0.0  ·  reality (服务器)
────────────────────────────────────────────────
  同步目录  /path/to/source
  监听地址  0.0.0.0:52345
  实例 ID   3b7b81ee
  进程 PID  62289
  日志      .local-mirror/logs/error.log (级别 error)
────────────────────────────────────────────────
```

## 工作原理

两端启动时各自扫描同步目录，并发计算文件的 blake3 哈希，目录树持久化到
bbolt（`.local-mirror/cache.db`）。重启时只重算 size 或 mtime 变过的文件，
并清理离线期间被删除的节点。

服务端用 fsnotify 监听变化。目录按活跃度打分：热门目录实时监听，冷门目录
降级为低频轮询并自适应退避——既把 watch 数量压在系统上限之下，也让空闲的
笔记本真正闲下来。变更目录记录在一小时的滚动日志里。

客户端不轮询。它发一个"等变更"请求然后阻塞，服务端一有变更立即应答，
无变更则最多挂 50 秒回个空包保活。变更通常两秒左右就到镜像端。正确性
从不依赖某次通知是否送达：每个请求都按时间区间查询变更日志，丢一次唤醒
下个来回自然捞回来。默认每 30 分钟一次的低频全量扫描是最终兜底，检测到
长时间休眠醒来后会强制执行一次。

文件差异按 blake3 哈希加大小判断，内容没变就绝不重传。传输分块进行，
按整文件哈希校验，再原子改名落盘——永远不会看到半截文件。下载中断后
分片保留在 `.local-mirror/partial/`，重试时从断点续传；期间源文件变了，
指纹对不上就整个重下。同目录内的重命名按哈希识别、本地直接改名而不重新
下载，mtime 也对齐源文件。中继用同一套引擎应用变更并立即唤醒自己的下游，
所以在传递链的每一跳上，重命名照样免重传、mtime 照样准确。

长轮询的往返本身就是心跳：TCP keepalive 在系统层探测死掉的对端，服务端
断开空闲超 90 秒的连接，并限制并发连接总数。服务端重启后，客户端在重连
握手中发现实例 ID 变了，会重建会话并做一次全量对账。

符号链接完全不追踪——不同步、不解引用。这是有意为之：解引用会把同步根
之外的内容泄露进镜像。详见 [SECURITY.md](SECURITY.md)。

### 局域网发现

`-r` 留空时，mirror/relay 通过 UDP 组播/广播探测局域网（查询-应答式，
空闲零流量）。交互终端下弹出列表选择；非交互环境恰好发现一台时自动连接。
设置了 `-k` 时探测带 keyed MAC，服务端直接无视未认证的扫描，不向扫描者
泄露同步路径。发现不跨路由器和 VPN 隧道——这类环境用 `-r` 直连。

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
客户端命中即不下载，`--allow-delete` 下也不删除。内置默认只有
`.local-mirror`、`.git`、`.DS_Store`——想跳过 `node_modules` 之类的请自行添加。

## 删除保护

同步会覆盖已存在的文件，`--allow-delete` 还会删多余的，所以真正要防的
失败模式是把工具指错目录。保护分三级：

1. **默认**——只同步不删除。关键路径直接拒绝启动：家目录、文件系统根，
   以及 `/etc`、`/usr` 等系统目录树**连同其子目录**（`-p /etc/nginx` 和
   `-p /etc` 一样被拒）。判定前先解引用符号链接，别名绕不过去。家目录
   内部的普通目录不受限制。
2. **`--allow-critical`**——解锁关键路径上的同步，仍不删除。已存在的文件
   首次被覆盖前，原件先复制到 `.local-mirror/backups/<相对路径>`。之后的
   同步绝不碰这份备份；复制失败则跳过该文件，绝不无备份地覆盖。
3. **`--allow-delete`**——启用删除。关键路径上必须与 `--allow-critical`
   同时给才生效；普通路径单独给即可。

在这套阶梯之外，服务端下发的每个路径都会校验必须落在同步根之内。`..`
穿越、绝对路径等一律在落盘前拒绝——恶意或有 bug 的服务端没办法写到、
删到你给定的目录之外。

## 加密

可选，走 Noise 协议（NNpsk0：X25519 + ChaCha20-Poly1305 + BLAKE2s）。
两端用 `-k` 配同一个口令即可获得双向认证和前向保密；口令不对或对端说
明文，握手当场失败。

只要不是可信局域网——公网、共享 Wi-Fi、多租户网络——务必 `-k` 加
显式 `-r` 一起用。不加密时握手无认证，而发现应答是明文且有放大特性，
所以发现功能只设计给可信局域网。细节见 [SECURITY.md](SECURITY.md) 的
审计记录。

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

每个任务是独立子进程，任务间零共享、互不拖累。日志按 `[任务名]` 前缀
汇聚。崩溃的任务指数退避重启（5 秒起封顶 60 秒）；退出码 2（目录不存在、
口令错这类配置问题）判为永久错误，只停该任务。父进程收到 SIGTERM/SIGINT
统一停全部子任务，整个可以直接跑成一个 systemd 服务。

几点实用说明：同机服务端任务共享 52345–52354 端口段（最多 10 个）；
mirror/relay 任务建议显式写 `realityip`（走发现时，扫到零台按可重试处理、
退避后再扫，兼容上游后启动；扫到多台是配置歧义，判永久错误）；`secret`
经环境变量传给子进程，不出现在 `ps` 里。

## systemd 部署

有 `-p` 就不用折腾工作目录。仓库带了单元文件示例：
[deploy/local-mirror.service](deploy/local-mirror.service)：

```bash
sudo cp local-mirror /usr/local/bin/
sudo cp deploy/local-mirror.service /etc/systemd/system/
# 编辑 ExecStart 里的模式/目录/上游地址后：
sudo systemctl daemon-reload
sudo systemctl enable --now local-mirror
journalctl -u local-mirror -f
```

口令用 `Environment=LOCAL_MIRROR_SECRET=...` 或 `EnvironmentFile=` 注入，
别写在 `-k` 参数里，免得出现在 `ps` 输出中。

## 运行时产物

全部在同步根下的 `.local-mirror/` 里（同步逻辑和 git 都忽略它）：

- `cache.db` — 持久化的目录树；大目录重启快，未变化的文件不重算哈希
- `logs/error.log` — 运行日志，单文件 10 MB 轮转，保留最近 3 个
- `partial/` — 中断下载的分片，等待续传
- `backups/` — 覆盖前备份，仅 `--allow-critical` 时产生
- `ignore` — 可选的忽略模式，与 `-i` 合并（改后重启生效）

## 构建

```bash
go build -o local-mirror ./cmd/local-mirror

# 交叉编译全平台到 dist/（版本号取 git describe）
./build.sh        # Linux / macOS
./build.ps1       # Windows
```

## 目录结构

```
cmd/local-mirror/   入口：参数校验、横幅、多任务监督
config/             flag 定义与 YAML 多任务配置
internal/
  network/          协议、服务端、客户端、端口探测、局域网发现
  tree/             目录树、bbolt 持久化、变更日志
  watcher/          热度评分监视与事件过滤
  safety/           关键路径检查、路径越界校验、覆盖前备份
  logger/           日志与按大小轮转
  tui/              发现列表的 raw-mode 选择器
  *.go              三种模式的编排逻辑
pkg/                小型共享工具：哈希、路径、终端样式
deploy/             systemd 单元与 YAML 示例
```

推送/长轮询架构的设计笔记在 [DESIGN.md](DESIGN.md)，已知限制和计划在
[TODO.md](TODO.md)。
