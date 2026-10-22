# local-mirror 对比测试：rsync 与 syncthing（2026-10）

2026-10-21 至 10-22 期间，在一台 Mac 本机回环和 Mac ↔ Debian 局域网两种环境里，把 local-mirror 与 rsync、syncthing 放在同一套数据集和场景下对比。累计运行约 6 小时，原始记录 1,257 条，全部在 [`data/`](data/)。

期间 local-mirror 做了三项性能改动，每项改完都在同一环境复测，所以这份数据同时记录了这三步各自的效果：

| 提交 | 改动 | 对应数据 |
|---|---|---|
| `4458f40`（v2.11.0） | 基线 | `loopback-tiny-*.jsonl` |
| `2951137` | 变更管线的防抖与批处理计时缩短 | `loopback-tiny-*.jsonl`、`loopback-small-kqueue.jsonl` |
| `b394743` | macOS 改用 FSEvents 监听 | `loopback-fsevents.jsonl` |
| `b44f192` | 目录 rollup 哈希改为增量重算 | `loopback-small-incremental.jsonl`、`lan-small.jsonl` |

## 结论

以最终版本（`b44f192`）在局域网两个方向上的结果为准（small 数据集，10 万个文件，各 3 轮）：

**领先**

- 单个变更的传播延迟三家最快：中位 1.51 秒，两个方向一致，最慢一次 2.2 秒。rsync 经 ssh 为 2.9–3.3 秒，syncthing（1 秒聚合）为 2.3 秒，但它的长尾到过 31 秒。
- 单个变更的 CPU 低一到两个数量级：两端合计 0.01 秒，rsync 1.4–1.8 秒，syncthing 0.3–0.5 秒。rsync 每次都要新建 ssh 连接并比对全部 10 万个文件，这部分开销与改了多少无关。
- 一次写入 1,000 个文件与删除它们，都是三家最快（2.1–2.6 秒 / 1.2–1.9 秒）。
- 对 syncthing 全面领先：首次同步快 8–18 倍，CPU 约为其 1/6。
- 空闲开销：5 分钟 0.04 秒 CPU（FSEvents 版在回环上测得，见第 3 节）。

**打平**

- 传输方向不影响增量性能：mac→debian 与 debian→mac 的单变更中位数都是 1.51 秒。
- 批量写入 1,000 个文件，rsync 只慢 0.8–1.8 秒。但 rsync 需要外部触发（cron、手动），触发间隔不计入这里的数字。

**落后**

- 首次同步输给 rsync：慢 1.8–2.7 倍（35–40 秒对 13–23 秒），CPU 是其 2.7–4.3 倍（67–72 秒对 15–26 秒）。CPU 在两端各占一半左右，源端 35–41 秒，副本端约 31 秒。这是目前唯一明显的短板，也是下一步优化的方向。
- 本机回环（不跨机器）时 rsync 的单次同步只要 0.6 秒，比 local-mirror 的 1.4–1.5 秒快。但这是在有人立即触发 rsync 的前提下。

## 测试环境

### 机器

| | Mac | Debian |
|---|---|---|
| 型号 | MacBook Air（Mac16,12），Apple M4，10 核 | 小主机，Intel N100，4 核 |
| 内存 | 24 GiB | 7.5 GiB（测试时约 3 GiB 被其他服务占用） |
| 系统 | macOS 27.0.1（26A434） | Debian 13.4 trixie，内核 6.12.74 |
| 文件系统 | APFS，内置 SSD | ext4，NVMe SSD（Fanxiang S500） |
| 网络 | Wi-Fi 6（802.11ax，5 GHz，160 MHz） | 2.5 GbE 有线 |
| 文件监控 | FSEvents（`b394743` 起），之前为 kqueue | inotify，`max_user_watches` = 60,295 |
| 背景负载 | 日常桌面使用 | 常驻 jellyfin、qbittorrent、PostgreSQL、MySQL、Redis 等 docker 服务，空闲时 load average 约 0.6 |

两台机器之间走同一个局域网，Mac 无线、Debian 有线。ping 往返 7–21 毫秒，偶尔尖峰到近 100 毫秒（Wi-Fi）。测试过程中实测吞吐约 800 Mbit/s。

### 被测程序

| 程序 | Mac | Debian | 说明 |
|---|---|---|---|
| local-mirror | 见上表各提交，`go build` | 同一提交，`GOOS=linux CGO_ENABLED=0` 交叉编译 | Go 1.27.1 |
| rsync | 3.5.1（Homebrew） | 3.4.1 | 跨机器时经 OpenSSH（Mac 10.3p1，Debian 10.0p2） |
| syncthing | 2.1.7 | 2.1.7 | 官方发布的二进制 |

## 数据集

由 `bench gen` 生成，布局、大小与内容全部由固定种子决定，任何机器上生成的字节都相同。manifest 记录每个文件的 sha256，用于校验。内容是不可压缩的伪随机数据，所有文件的修改时间固定为 2026-01-01。

| 名称 | 内容 | 用途 |
|---|---|---|
| tiny | 500 个文件，1–16 KiB，25 个目录 | 冒烟测试、版本间快速对比 |
| small | 100,000 个文件，1–16 KiB，5,000 个目录（3 层），共 0.81 GiB | 主测试集 |

mixed（20,000 个文件，1.47 GiB）和 large（4 × 1 GiB）也有定义。回环全量测试原计划跑这两组，但在 small 的 3 轮之后中止，所以这里没有它们的数据。

## 测试场景

每轮（trial）开始前：清空副本目录，把源数据集整个读一遍，让页缓存保持热态。每轮结束后，撤销对源数据集的全部改动，并删掉被测程序在源目录里写入的状态目录。下一轮开始前会校验源数据集逐字节无误。偶数轮的工具执行顺序反过来，以抵消先后顺序带来的偏差。

| 场景 | 过程 | 计时 |
|---|---|---|
| initial（首次同步） | 空副本 → 启动程序（rsync 则执行一次） | 从启动到副本里出现全部文件且大小一致；之后对每个文件做 sha256 全量校验（不计时） |
| idle（空闲开销，持续型程序） | initial 完成后等 30 秒，再观察 5 分钟 | 窗口内两端进程树的 CPU |
| noop（空跑，rsync） | 源与副本已经一致时再跑一次 rsync | rsync 本身的耗时与 CPU |
| change（单个变更） | 每轮 30 个变更，按 新建 → 修改 → 删除 循环。修改和删除的对象从数据集里随机挑选，大小 1–16 KiB。随机序列由轮次决定，每个工具拿到的变更完全相同 | 从源端写完到副本出现内容一致的文件（sha256 比对）或文件消失。持续型程序每个变更之间间隔 2 秒加一个随机的 0–1 秒，避免变更总落在秒边界的同一相位上 |
| burst（批量） | create：在一个新目录下写入 1,000 个 1–16 KiB 的文件（分 10 个子目录）；delete：再整个删掉 | create 从写第一个文件开始，到副本里全部到齐；delete 从开始删除到副本里全部消失。create 之后对 1,000 个文件逐个校验内容 |

rsync 是一次性工具，在 change 和 burst 场景里，源端改完后立即执行一次 rsync，计的是这一次的耗时。实际使用中还要加上 cron 或手动触发的间隔，这部分没有计入。

### 计量方法

- **CPU**：持续型程序每 250 毫秒对其进程树（含子进程）采样一次，macOS 用 `ps`，Linux 读 `/proc`，窗口内的 CPU 时间差即为结果。精度受 `ps` 的 0.01 秒分辨率限制，所以单变更显示的 0.01 秒实际上是"不超过约 0.01 秒"。rsync 用进程退出时的 rusage（含它派生的 ssh），跨机器时再加上远端 rsync 的 CPU：远端用 `--rsync-path` 包装脚本，以 bash `times` 记录。
- **两端分开计**：跨机器时，记录里另有 `cpu_src_s` 和 `cpu_dst_s` 两个字段，分别是源端与副本端的 CPU。
- **内存**：记录了峰值 RSS，但 **macOS 上的数字不可信**。local-mirror 的 bbolt 数据库通过 mmap 读入的页，以及 Go 运行时用 `MADV_FREE` 归还的堆，都会算进 RSS，使数字远大于实际占用。要比较内存，需要改用 `phys_footprint`（尚未做），所以本文不对内存下结论。
- **跨机器的计时**：所有时间戳都取自驱动端（Mac）。源端或副本端在远端时，会多计或少计半个往返，也就是几毫秒，相对 1 秒以上的延迟可以忽略。

### 被测程序的配置

**local-mirror**

```
源端：  local-mirror --send    -p <src> -k <固定密钥> (--listen | --connect <副本端地址>)
副本端：local-mirror --receive -p <dst> -k <固定密钥> --allow-delete (--connect <源端地址> | --listen)
```

回环时源端监听；跨机器时远端监听、Mac 去连，Mac 不用开防火墙。监听端绑好端口后才启动拨号端，避免首次拨号失败触发 3 秒的重连退避，否则测出来的是退避时间。

**rsync**

```
回环：  rsync -a --delete <src>/ <dst>/
跨机器：rsync -a --delete --rsync-path=<计时包装脚本> <src>/ debian:<dst>/     （或反向拉取）
```

每次执行都新建一个 ssh 连接，不复用 ControlMaster。这符合 rsync 用 cron 跑的典型用法。

**syncthing**：两个实例，源端文件夹为 `sendonly`，副本端为 `receiveonly`。全局发现、本地广播、中继、NAT 穿透、使用统计、崩溃上报和自动升级全部关闭，`rescanIntervalS=3600`，`compression=metadata`。

- `syncthing`：文件监控聚合延迟 `fsWatcherDelayS` 用默认值 10 秒。
- `syncthing-tuned`：`fsWatcherDelayS=1`，这是允许的最小值。局域网测试只跑了这一档。

## 结果

除特别说明外，表中数值都是中位数；单变更场景同时给出 p90 和最大值。

### 1. 局域网 mac ↔ debian（`b44f192`，small，各 3 轮）

数据：[`data/lan-small.jsonl`](data/lan-small.jsonl)，测于 2026-10-22 22:05–23:49。594 条记录，没有超时，全部通过校验。

**mac → debian**（Mac 为源）

| 场景 | local-mirror | rsync（ssh） | syncthing-tuned |
|---|---|---|---|
| 首次同步 | 35.0 s | **13.0 s** | 297 s |
| 　CPU（源 + 副本） | 67.1 s（35.5 + 31.2） | **15.5 s**（7.6 + 7.9） | 446 s（95.5 + 350） |
| 单变更 p50 / p90 / max | **1.51 / 1.85 / 2.18 s** | 3.26 / 3.48 / 3.69 s | 2.33 / 2.40 / 31.0 s |
| 　每次的 CPU | **0.01 s** | 1.76 s | 0.51 s |
| 批量写入 1,000 个文件 | **2.05 s** | 3.84 s | 4.45 s |
| 　CPU | **0.95 s** | 1.93 s | 4.21 s |
| 批量删除 1,000 个文件 | **1.17 s** | 3.38 s | 7.07 s |
| 　CPU | **0.14 s** | 1.78 s | 5.81 s |

**debian → mac**（Debian 为源）

| 场景 | local-mirror | rsync（ssh） | syncthing-tuned |
|---|---|---|---|
| 首次同步 | 40.5 s | **23.0 s** | 752 s |
| 　CPU（源 + 副本） | 72.2 s（41.2 + 30.9） | **26.3 s**（4.1 + 22.1） | 439 s（212 + 225） |
| 单变更 p50 / p90 / max | **1.51 / 1.84 / 2.07 s** | 2.85 / 3.07 / 4.49 s | 2.30 / 2.38 / 2.60 s |
| 　每次的 CPU | **0.01 s** | 1.42 s | 0.32 s |
| 批量写入 1,000 个文件 | **2.64 s** | 3.48 s | 7.29 s |
| 　CPU | **1.29 s** | 1.67 s | 3.84 s |
| 批量删除 1,000 个文件 | **1.87 s** | 2.96 s | 13.98 s |
| 　CPU | **0.18 s** | 1.49 s | 13.60 s |

几点观察：

- local-mirror 首次同步 debian 作源时比 Mac 作源时慢约 5 秒，多出的 CPU 都在源端（41.2 对 35.5 秒）：N100 上建树和算哈希比 M4 慢，同时还在跑其他服务。副本端的 CPU 两个方向几乎一样（约 31 秒）。
- rsync 首次同步的 CPU 主要在接收端（Mac 作副本时 22 秒）。
- syncthing 在 Mac 作副本时首次同步慢 2.5 倍（752 秒对 297 秒），批量删除也慢一倍。它的 CPU 两个方向差不多，所以时间主要耗在等待上，而不是计算上。
- syncthing 的单变更在 mac→debian 方向有一次 31 秒的长尾，其余都在 2.3–2.6 秒之间。

### 2. 本机回环，四个工具（`2951137`，small，3 轮）

数据：[`data/loopback-small-kqueue.jsonl`](data/loopback-small-kqueue.jsonl)，测于 2026-10-21 22:11 – 10-22 01:30。这一组用的是改用 FSEvents **之前**的版本，local-mirror 当时在 macOS 上用 kqueue 监听目录。

| 场景 | local-mirror | rsync | syncthing-tuned | syncthing（默认） |
|---|---|---|---|---|
| 首次同步 | 40.2 s | **21.5 s** | 726 s | 693 s |
| 　CPU | 66.4 s | **29.0 s** | 327 s | 317 s |
| 空闲 5 分钟的 CPU / rsync 空跑 | 1.04 s | 0.62 s 耗时，1.01 s CPU | 0.41 s | 9.90 s |
| 单变更 p50 / p90 / max | 26.6 / 28.7 / 420 s ⚠ | **0.60 / 0.62 / 0.63 s** | 2.32 / 2.66 / 22.7 s | 7.72 / 16.8 / 44.2 s |
| 　每次的 CPU | 0.64 s | 0.97 s | 0.26 s | 0.47 s |
| 批量写入 1,000 个文件 | 28.7 s ⚠ | **0.90 s** | 6.57 s | 11.6 s |
| 批量删除 1,000 个文件 | 1.55 s | **0.70 s** | 7.55 s | 16.7 s |

⚠ 这组 local-mirror 的单变更延迟呈双峰分布：约 1/3 在 1–2 秒，其余集中在 20–28 秒，另有 1 次超过 7 分钟的超时上限。原因是 kqueue 每个目录占一个文件描述符，small 的 5,000 个目录超过了监听预算，多数目录被降到第二级的轮询，那里的变更要等下一次轮询才能发现。这是改用 FSEvents（`b394743`）的直接动因，改后见下一节。

rsync 在回环下没有 ssh 开销，单次同步只要 0.6 秒，是这一组里单变更最快的；跨机器后这一优势消失（见第 1 节）。

### 3. local-mirror 自身的改进（本机回环，small）

| 版本 | 单变更 p50 / p90 / max | 每次的 CPU | 批量写入 / 删除 | 空闲 5 分钟的 CPU | 首次同步 |
|---|---|---|---|---|---|
| `2951137`（kqueue） | 26.6 / 28.7 / 420 s | 0.64 s | 28.7 / 1.55 s | 1.04 s | 40.2 s |
| `b394743`（FSEvents） | 1.65 / 2.10 / 2.23 s | 0.20 s | 2.14 / 1.57 s | 0.04 s | 41.6 s |
| `b44f192`（增量 rollup） | **1.44 / 1.81 / 2.05 s** | **0.01 s** | 2.18 / 1.52 s | — | 41.1 s |

数据：`loopback-small-kqueue.jsonl`（3 轮）、[`data/loopback-fsevents.jsonl`](data/loopback-fsevents.jsonl)（1 轮）、[`data/loopback-small-incremental.jsonl`](data/loopback-small-incremental.jsonl)（1 轮）。

- **FSEvents**：整棵树只用一个事件流，所有目录都能实时监听。单变更从 26.6 秒降到 1.65 秒，空闲 CPU 从 1.04 秒降到 0.04 秒，后者是因为不用再轮询。
- **增量 rollup**：之前每次变更都要把全树 5,000 个目录的 Merkle 哈希重算一遍，每次 0.2 秒 CPU，且随树的大小线性增长。改成只重算变更路径上的祖先目录后，降到不超过 0.01 秒。

### 4. 小数据集上的延迟与批量（tiny，本机回环）

数据：[`data/loopback-tiny-latency.jsonl`](data/loopback-tiny-latency.jsonl)（各 30 个变更）、[`data/loopback-tiny-burst.jsonl`](data/loopback-tiny-burst.jsonl)（各 3 轮）。

| | v2.11.0 | `2951137` | rsync | syncthing-tuned | syncthing（默认） |
|---|---|---|---|---|---|
| 单变更 p50 / p90 / max | 4.31 / 4.78 / 4.90 s | 1.44 / 1.80 / 1.90 s | — | 2.31 / 3.31 / 4.23 s | 7.74 / 12.0 / 30.1 s |
| 批量写入 1,000 个文件 | 3.03 s | 1.99 s | 0.35 s | 6.40 s | 11.3 s |
| 批量删除 1,000 个文件 | 3.50 s | 1.52 s | 0.15 s | 2.19 s | 13.2 s |

`2951137` 的改动：把文件事件的批处理窗口和变更日志的落盘节流从秒级缩短到 250–500 毫秒，单变更延迟从约 4.3 秒降到约 1.4 秒。

local-mirror 现在剩下约 1.4 秒的延迟下限，大致由三部分构成：事件防抖与批处理窗口，变更日志按秒定型（约 0.5 秒），再加一次拉取的往返。若要把后一项做到毫秒级，需要改协议，暂未做。

## 已知局限

- **数据集单一**：只跑了 small（大量小文件），大文件和混合数据（mixed、large）没有测到。
- **轮数**：局域网与回环的主测试各 3 轮；FSEvents 和增量 rollup 两个版本各只跑了 1 轮（30 个变更）。
- **Debian 不是空闲机器**：它上面的常驻服务会给副本端或源端的数字带来噪声。
- **Mac 走 Wi-Fi**：往返时延有尖峰。单变更这类小数据量场景受影响有限，首次同步的吞吐可能被低估。
- **内存未比较**：macOS 的 RSS 不可信，原因见"计量方法"。
- **rsync 的延迟不含触发间隔**：表中是"改完立刻执行一次"的理想情况。
- **CPU 分辨率**：约 0.01 秒，单变更的 0.01 秒应读作"小于等于"。
- **没有收录的数据**：
  - 早期一组 tiny 延迟数据：当时变更间隔固定为整秒，与 local-mirror 按秒处理的节奏发生相位锁定，测得的延迟几乎都是整秒，加入随机间隔后重测，即第 4 节。
  - 几组只用来确认流程能跑通的冒烟测试。

## 复现

```bash
cd bench
go build -o bench ./cmd/bench
./bench gen -ws <ws> -datasets small
```

本机回环：

```bash
./bench run -ws <ws> -out loopback.jsonl -env loopback -local-mirror <local-mirror> \
  -tools local-mirror,rsync,syncthing-tuned,syncthing -datasets small \
  -scenarios initial,steady,change,burst -trials 3 -idle 5m
```

跨机器：先把 Linux 版的 `bench` 与 `local-mirror` 放到远端的 `~/lm-bench/`（syncthing 同理）。Mac 作源时远端不需要数据集；`-remote-src` 时需先在远端执行 `bench gen`。

```bash
./bench run -ws <ws> -out lan.jsonl -env "lan mac→debian" -local-mirror <local-mirror> \
  -remote <ssh 主机名> -remote-addr <远端局域网 IP> \
  -tools local-mirror,rsync,syncthing-tuned -datasets small \
  -scenarios initial,change,burst -trials 3 -changes 30
# 反方向加 -remote-src
```

生成图表：

```bash
./bench chart -out report.md data/*.jsonl
```

## 数据格式

`data/*.jsonl` 每行一条记录：

| 字段 | 含义 |
|---|---|
| `env` | 环境与版本标签，例如 `lan mac→debian`、`loopback b394743 (FSEvents)` |
| `host` / `os` / `cpu` | 机器信息，两端不同时写成"源 → 副本" |
| `tool` / `tool_version` | 被测程序；两端版本不同时写成"源/副本"；local-mirror 从源码构建，版本显示为 `dev`，对应的提交见 `env` |
| `dataset` / `scenario` / `trial` / `op` | 数据集、场景（`initial`、`idle`、`noop`、`change`、`burst`）、轮次、变更类型 |
| `wall_s` | 耗时（秒） |
| `cpu_s` | 两端合计 CPU（秒） |
| `cpu_src_s` / `cpu_dst_s` | 跨机器时源端与副本端各自的 CPU |
| `peak_rss_bytes` / `avg_rss_bytes` | 进程树 RSS 之和（macOS 上不可信，见"计量方法"） |
| `timed_out` / `verified` | 是否超时、副本内容是否通过校验 |
| `time` | 记录时间（UTC） |

收录时做了两处改动：`env` 改成了可读的标签，`host` 字段做了脱敏；其余字段与 bench 原始输出一致。
