# 配置化与服务化设计 · v2.1.0

> ## 📋 历史设计记录（ADR）— 已全部实现
>
> **状态订正（2026-08-07）**：本文档原标「设计阶段，未实现」，但四支柱 P1~P5 早在
> **v2.1.0 / v2.2.0 就已实现并上线**（正文里多处 “已实现并验证（2026-08-03）” 即当时的落地标记）。
> 保留本文是因为它记录了大量**决策理由**（为什么砍掉配置自动发现、为什么放弃 deb/rpm、
> 为什么密钥走 stdin 而非子进程重读配置），对后续维护有价值。
>
> **⚠️ 这是设计决策记录，不是当前行为的权威描述。** 判断「现在代码到底怎么跑」请以
> **README + 源码**为准；各章节锚点（§P1.4、§P2.3、§P4.3 等）被代码注释引用，故文件名与
> 编号保持不动。个别细节可能已被后续变更覆盖——例如 §P4.3 的 `ReadWritePaths` 生成，
> 现已知在含空格/特殊字符的路径上存在转义缺陷（见 `docs/CODEX_CODE_AUDIT.md` SVC-01）。
>
> ---
>
> 原始目标：让 local-mirror 从「一堆命令行旗子拼出来的进程」变成
> **「装完就能 `systemctl start local-mirror` 的服务」**——
> 参照 xray / sing-box 的心智：**二进制一个名、配置一个文件、服务一条命令**。

## 0. 四支柱与依赖顺序

| # | 支柱 | 一句话 | 角色 |
|---|---|---|---|
| **P1** | 配置默认路径 | 各平台约定一个 `config.yml` 落点（**不做自动发现**） | **基座**：没有它，`service install` 不知道把配置建在哪 |
| **P2** | 密钥归位 | key 可写进配置，`LOCAL_MIRROR_SECRET` 用户面退休 | 让配置文件能承载**全部**配置 |
| **P3** | 单任务不 fork | 配置里只有一个 task 时直接跑，不起监督父进程 | 服务化的进程模型代价必须为零 |
| **P4** | `service` 子命令 | 跨平台装服务 + **自动创建空白配置** | 用户最终看到的那个「一条命令」 |
| **P5** | 分发形态 | ~~deb/rpm~~ → **纯静态二进制 + install.sh 代理 service install** | 覆盖任何 Linux/macOS/Windows |

**依赖顺序**：P1 是基座（P4 要靠它才知道把配置建在哪、`ExecStart` 里写什么路径）；
P2 独立、非破坏，可与 P1 并行；P3 独立（纯进程模型优化）；P4 依赖 P1+P2；
P5 依赖 P1~P4 全部就位。

**动机链**：想要通用 unit → `ExecStart` 里的两个路径都必须是**固定常量**（二进制路径 + 配置路径）
→ 配置路径得有个约定落点（P1）→ 密钥也得能放进配置里，否则还要额外的 EnvironmentFile（P2）
→ 但配置化不能为此多付一个进程（P3）→ 用户不该手写 unit、也不该手工创建配置目录（P4）
→ 包管理器应该直接给可用品（P5）。

---

## P1. 配置默认路径 —— 基座

> **设计取舍（重要）**：本节曾设计过一套四层「配置自动发现」链
> （`--config` → CWD → 用户级 → 系统级）。**已推翻，改为「只有显式 `--config`」**。
> 推翻理由见 P1.5。

### P1.1 现状与缺口

现状：`--config` **必须显式给路径**，没有默认位置的概念（`config/config.go:401`）。

缺口不在「不会自动找」，而在**没有一个众所周知的落点**——用户不知道配置该放哪，
`service install` 也没有约定的地方去创建它。这才是生产 unit 里塞满旗子的根本原因：
没有配置文件可指，只能把 `--receive --listen -p … -l warn --allow-delete` 全焊进 `ExecStart`。

### P1.2 跨平台约定落点（定稿）

**这不是搜索路径**，而是 `service install` 创建配置、并写进服务描述文件的**约定落点**
（见 P1.3）。各平台按**自己的原生规范**给路径，而不是强行统一：

| 平台 | 系统级（服务用） | 用户级 |
|---|---|---|
| **Linux** | `/etc/local-mirror/config.yml` | `$XDG_CONFIG_HOME/local-mirror/config.yml`，回退 `~/.config/local-mirror/config.yml` |
| **macOS** | `/opt/homebrew/etc/local-mirror/config.yml`（Apple Silicon brew 前缀，优先）<br>`/usr/local/etc/local-mirror/config.yml`（Intel brew / 手工安装） | `~/.config/local-mirror/config.yml` |
| **Windows** | `%ProgramData%\local-mirror\config.yml` | `%AppData%\local-mirror\config.yml` |

**两个决策要点：**

**① macOS 用 `~/.config` 而不是 `~/Library/Application Support`。**
Go 的 `os.UserConfigDir()` 在 macOS 上返回 `~/Library/Application Support`，我们**不直接用它**，
自己判平台。理由：local-mirror 是 Unix 风格的守护型 CLI，不是 GUI 应用；
`Library/Application Support` 是给 GUI app 的。更重要的是——**保持 mac 与 Linux 运维对称**，
同一套文档、同一套排查路径，两端心智一致。

**② macOS 系统级要探两个 brew 前缀。** Apple Silicon 是 `/opt/homebrew`，Intel 是 `/usr/local`，
只探一个会在换机器时莫名找不到配置。

### P1.3 配置来源：只有一条

```
--config <path>          唯一来源，必须显式给
```

**没有自动发现、没有搜索链、没有隐式回落。** P1.2 那张表里的路径**不是搜索位置**，
而是 `service install` 创建配置文件、并写进服务描述文件的**约定落点**。

用户手动跑就是 `local-mirror --config=./local-mirror.yml`，显式、清楚、够用。

### P1.4 规则约束（只剩两条）

| # | 规则 | 理由 |
|---|---|---|
| **R1** | `--config` 指向的文件不存在/不可读 → **报错 exit 2**，不做任何回落 | 静默回落是最难排查的坑：你以为在用 A 配置，实际跑的是 B |
| **R4** | **拒绝加载位于任一 task 同步根内部的配置文件** → exit 2 | **local-mirror 独有的硬约束**，见 P2.4 |

> 编号保留 R1/R4 是为了对齐上一版设计的讨论记录；R2/R3/R5/R6 随发现机制一并作废。

**已实现并验证**（2026-08-03）：两条规则都落在 `config.LoadMultiConfig` 里
（配置的唯一校验入口，调用方无需记得单独校验），失败均 exit 2。
R4 复用 `safety.IsInside`（新导出），两边路径都解引用真实路径，
软链指向同步根内部同样拦得住；同前缀的兄弟目录（`/x/rootX` vs `/x/root`）不误判。

### P1.5 为什么推翻「自动发现」

上一版设计了 `--config` → CWD → 用户级 → 系统级的四层发现链。推翻它的三个理由，
一条比一条硬：

**① 「零参数 unit」这个目标本身就是错的。**
`service install` **是它自己创建配置文件的**，当然知道路径——它完全可以把
`--config /etc/local-mirror/config.yml` 直接写进生成的 unit。这样 unit 依然是通用模板
（路径是固定常量，永不随版本变），而且**更好**：`systemctl cat local-mirror` 一眼看到
用的哪个配置，自解释。零参数反而藏起了这个信息。

**② 服务上下文里，用户级路径的行为不可预测。**
systemd 里 `User=xwvike` 的服务，`HOME` 可能设也可能没设（取决于 unit 写法与 systemd 版本），
`~/.config/local-mirror/config.yml` 这层发现在服务上下文里会**时灵时不灵**。
显式路径直接免疫这个问题。

**③ 唯一剩下的人机便利理由，`--status --all` 已经覆盖了。**
原本给发现机制找的最大理由是「部署完之后 `local-mirror --status` 能直接看状态，
不用记路径」。但 `--all` 旗子（`config/config.go:386`）本来就是：

> `with --status or --heat: discover and show every local-mirror running on this host`

**`local-mirror --status --all` 扫本机所有实例，不需要任何配置文件。** 理由自己不成立。

**净收益**：CWD 撞名（`config.yml` 是极其通用的名字，几乎每个项目根目录都有一个）、
`HOME` 不确定、发现顺序歧义——**三个坑一起消失**，规则从六条降到两条，
`cliFlagsSet()` 也不需要为「防止杂散配置劫持命令」增加任何判定。

> **这是纯粹的减法**：将来真需要「裸跑 `local-mirror` 找默认配置」，
> 随时可以作为**加法**补上（读 P1.2 表里的固定路径即可），不会与本设计冲突。
> 先不做，是因为现在找不到它必须存在的理由。

---

## P2. 密钥归位 —— 让配置能承载全部

### P2.1 现状盘点（先纠正一个常见误解）

全仓扫描，应用真正读取的**项目环境变量只有一个**：

| 位置 | 变量 | 性质 |
|---|---|---|
| `config/config.go:374` | `LOCAL_MIRROR_SECRET` | **唯一**的用户面项目环境变量 |
| `cmd/local-mirror/supervisor.go:144` | `LOCAL_MIRROR_SECRET` | 同一个变量，父进程传子进程的**内部管道** |
| `pkg/termstyle/termstyle.go:20` | `NO_COLOR` / `TERM` | **行业通用约定**（no-color.org），不是项目配置 |

所以并不存在「好几种环境变量」需要清理。但**砍掉用户面 env 的方向依然成立**，
理由是下面的 P2.2：有了配置文件，env 就变成了纯粹多余的第二条路径。

### P2.2 密钥的三条来源与优先级

`TaskConfig.Secret`（`config/multiconfig.go:32`，`secret:` 字段）**本来就存在**，
拼图早就齐了，只差 P1 的默认路径把它接起来。

定稿优先级：

```
-k / --secret 显式旗子   >   配置文件的 secret:   >   <同步根>/.local-mirror/key
```

- **`LOCAL_MIRROR_SECRET` 直接移除**，不走废弃周期（目前只有自己在用，没有外部兼容负担）。

### P2.3 移除 env 的技术前提：先换掉父子传输通道

⚠️ **不能直接删 `config.go:374`。** 那行 `os.Getenv` 同时是**子进程读密钥的唯一入口**：
多任务模式下父进程在 `supervisor.go:144` 设这个变量、子进程靠 374 行读回来。
直接删 = 多任务模式的密钥传递断掉。

所以「移除 env」实际要做的是**换一条父→子通道**。定稿方案：**密钥走 stdin 管道**。

```go
// 父进程：密钥写进子进程 stdin，不进 argv、不进 env
cmd.Stdin = strings.NewReader(secret + "\n")

// 子进程：--secret-stdin 时从 stdin 读一行作为密钥
```

其余字段仍走 argv（`taskArgs` 不变）。

**为什么是 stdin：**
- 不出现在 `ps` 的命令行里（保住 `supervisor.go:139` 原始注释的意图）
- 也不出现在 `/proc/<pid>/environ`——比 env **更**严格
- 子进程本来就不读 stdin，零冲突
- **完全不改变现有语义**，约 10 行改动

**被否决的替代方案：子进程自己重读配置**（父只传 `--config <path> --task <name>`）。
架构上更漂亮（配置成为唯一事实源、连 argv 序列化都不需要），但它把
「修改配置文件需重启父进程整体生效」这个**已文档化的行为**改掉了：
子进程崩溃重启会读到新配置，产生「部分任务跑新配置、部分跑旧配置」的不一致窗口。
为了删一个环境变量付这个代价不划算。可作为将来单独的重构议题。

### P2.4 为什么 keyfile 不能删（xray 类比的边界）

现在 key 在 `<同步根>/.local-mirror/key`，`keyfile.go` 开头写明了三条理由：

> `.local-mirror` 是强制忽略项（key 绝不会被同步）、不依赖 `$HOME`、每根一把 key = 每链独立身份

这里有一个 **xray 完全不存在的约束**：**local-mirror 的工作目录是要被复制到另一台机器的。**
密钥文件只要落在同步根内且未被强制忽略，就会**被镜像到对端**。xray 的 `config.json`
没有这个风险，所以「密钥全固化进 config」这个结论不能直接照搬。

由此得到 **R4**：**拒绝加载位于任一 task `path` 内部的配置文件**。
`/etc/local-mirror/config.yml` 天然满足；用户若把配置放进同步目录，必须硬报错拦住。

**同时保留 keyfile 作为兜底**，不删。理由：拨号端首次 `-k` 之后 `keyfile.Save()` 会把 key 存下来，
之后**再不用给 key**——这是很舒服的零配置体验（`local-mirror --send --connect host` 就跑）。
全砍到配置文件意味着「想跑必须先写配置」，对 CLI 直接用法是明显退步。
**配置化是给服务用的，不该拖累随手一跑。**

---

## P3. 单任务不 fork

### P3.1 现状

`--config` **必进监督模式**（`main.go:380` → `runSupervisor`）：父进程 + 每任务一子进程。
单任务场景 = **2 个进程**。这对「配置化服务」是纯粹的成本：
多一次进程调度、多一层信号转发，且 `pgrep`/`pkill` 的锚点习惯要重新捋
（历史上这个锚点问题已经踩过三次）。

### P3.2 方案：复用 `taskArgs`，零漂移

配置解析完后：

```
len(tasks) == 1  →  单进程直跑（不 fork）
len(tasks) >  1  →  监督模式（现状不变）
```

**实现要点**：不要手写一个逐字段写 `config.*` 全局变量的版本——
那会和 `taskArgs()` 形成**两份必然漂移的映射**（加了字段只改一处）。
改用**在进程内重解析**，落成 `applySingleTask()`（`supervisor.go`，紧邻 `taskArgs`）：

```go
func applySingleTask(t config.TaskConfig) {
	_ = flag.CommandLine.Parse(taskArgs(t))  // 复用多任务路径同一个映射函数
	*config.Secret = t.Secret                // 密钥单独赋值，绝不进 argv
}
// 调用后落回既有的单实例主流程，与命令行直接给旗子完全同路
```

重解析后 `flag.Visit` 会把这些旗子报为「已显式给出」，正是所需——
下游 `resolveDirection()` 等校验逻辑全部原样复用，无需任何特判。

**已实现并验证**（2026-08-03）：单任务配置启动后进程数 = 1（多任务仍为 1 父 + N 子）；
输出不再带 `[task-name]` 前缀（无父进程转发即为免 fork 的直接证据）；
单测 `TestApplySingleTaskMapsEveryField` 逐字段断言映射完整，防止将来加字段漏落。

好处：**TaskConfig → 配置** 只有一处映射，单/多任务两条路径永远一致；
参数校验逻辑也完全复用既有的那套。

**前置条件已满足**：`main.go:356-370` 已经强制「`--config` 模式下不得给其他旗子」，
所以重解析时其余旗子必然还是默认值，不会有残留污染。

---

## P4. `service` 子命令 —— 用户看到的那「一条命令」

### P4.1 形态（定稿）

```
local-mirror service install [--system|--user] [--now] [--config <path>]
local-mirror service uninstall
local-mirror service status
```

**架构影响（需要注意）**：项目**目前是纯 flag、零子命令**架构。
引入子命令需要在 `flag.Parse()` **之前**做一层分发：

```go
// main() 开头，flag.Parse() 之前
if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
    dispatchSubcommand(os.Args[1], os.Args[2:])   // 不返回
}
```

判定用「argv[1] 不以 `-` 开头」，保证 `local-mirror --status` 这类既有用法**零影响**。

### P4.2 install 做的四件事

1. **创建配置目录**（0755）与**空白配置文件**（0600），文件已存在时**绝不覆盖**。
2. **生成服务描述文件**，`ExecStart` = `os.Executable()` 拿到的**当前二进制真实路径**
   + **显式 `--config <刚创建的配置路径>`**。
3. **注册服务**（`daemon-reload` / `launchctl bootstrap`）。
4. **打印配置文件路径**，明确告诉用户「去编辑这个文件，然后 `systemctl start local-mirror`」。

> 这直接兑现「用户只用编写，不用创建路径和文件」——目录、空配置、服务文件全部由 install 生成。

**空白配置模板**（写进新建的 `config.yml`，全部注释掉，用户取消注释即可）：

```yaml
# local-mirror 配置文件
# 编辑后执行:  systemctl start local-mirror   (mac: launchctl kickstart …)
#
# tasks:
#   - name: backup
#     receive: true          # 本机是汇（数据流入）
#     listen: true           # 等对端拨入
#     path: /srv/backup      # 同步根（必填）
#     allow_delete: true     # 忠实镜像（含删除）
#     loglevel: warn
#     secret: <把 --gen-key 生成的 key 贴到这里>
```

### P4.3 各平台落点

| 平台 | `--system`（默认？） | `--user` | 注册命令 |
|---|---|---|---|
| **Linux** | `/etc/systemd/system/local-mirror.service`（**默认**，需 root） | `~/.config/systemd/user/local-mirror.service` | `systemctl daemon-reload` |
| **macOS** | `/Library/LaunchDaemons/com.xwvike.local-mirror.plist`（需 root） | `~/Library/LaunchAgents/…plist`（**默认**） | `launchctl bootstrap` |
| **Windows** | 见 P4.4 | — | — |

平台默认不同是**有意为之**：Linux 服务的常态是系统级 unit；macOS 上守护用户自己的目录，
LaunchAgent（用户级）才是常态，而且不需要 root。
**macOS 的 `--system`（LaunchDaemon）仍然支持**，只是不做默认——它需要 root，
且以 root 身份访问用户目录会带来权限与 TCC（隐私授权）的额外麻烦。

> ⚠️ **Debian 上 `--user` 不可用**：该机 PAM 不建用户会话，`systemctl --user` 用不了，
> 只能走系统级 unit + `User=`。install 应当检测并给出明确报错，而不是生成一个跑不起来的 unit。

生成的 Linux unit（**显式 `--config`**，见 P1.5 ①）：

```ini
[Unit]
Description=local-mirror directory sync
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=<当前用户>
ExecStart=/usr/bin/local-mirror --config /etc/local-mirror/config.yml
Restart=on-failure
RestartSec=5s
NoNewPrivileges=true
ProtectSystem=full
ReadWritePaths=<各 task 的 path>

[Install]
WantedBy=multi-user.target
```

**这仍然是通用模板**：二进制路径固定、配置路径固定，两者都不随版本或机器变化，
每台机器上生成的 unit 完全一致。而显式写出配置路径让 `systemctl cat local-mirror`
自解释——比零参数信息量更大。

**`ReadWritePaths` 的生成规则**：逐个 task 的 `path` 列出。
若某个 task 的根落在关键路径上（复用 `safety.IsCriticalRoot`），
**整个跳过 `ProtectSystem`/`ReadWritePaths` 加固并打印提示**，而不是生成
`ReadWritePaths=/` 这种把加固削成零、却看起来像有加固的危险规则。
宁可明确地不加固，也不要假装加固。

注意这里带上了 `ProtectSystem=full`，并按配置里的 `path` 自动生成 `ReadWritePaths`。

### P4.3.1 实现状态（2026-08-03）

**已实现**：`cmd/local-mirror/service.go`，子命令分发在 `main()` 里 `flag.Parse()` 之前，
**只精确匹配 `service` 这一个词**——不能用「argv[1] 不以 `-` 开头」判定，
那会把位置糖 `local-mirror ./dir @peer` 里的 `./dir` 误当成子命令。
额外提供 `--dry-run`：打印将写入的内容与将执行的命令，不碰系统。

**已验证**：12 个单测覆盖两个平台的产物（systemd 系统级/用户级、launchd 转义与
`plutil -lint` 真校验）、加固开关随 `RWPaths` 联动、关键路径跳过加固、
空白配置绝不覆盖已有配置、落点解析（临时 HOME）；真机跑通 `service status`、
`install --dry-run`、`uninstall --dry-run`、未知动作报错。

⚠️ **尚未做过真实的 live install**：生成的 launchd label 与 systemd unit 名
都与现有生产服务同名，真装会顶掉生产。live 安装留到迁移那一步做（那时替换生产正是目的）。

### P4.3.2 第三种 init：procd（OpenWrt）—— 2026-08-03 补齐

`detectInit()` 按 `/run/systemd/system`（systemd 的权威判据）→ `/sbin/procd` 依次判定，
darwin 直接是 launchd。procd 落点 `/etc/init.d/local-mirror`（**0755，init 脚本要可执行**），
注册走 `<script> enable`（建 rc.d 软链），日志进 `logread`。

**实测得到的两条硬约束**（OpenWrt 24.10 的 `/lib/functions/procd.sh`）：
- `_procd_set_param` **不支持 `user`/`group`** —— procd 下服务只能以 root 运行。
  因此 `--run-as` 在该平台**直接报错**，而不是生成一个会被静默忽略的参数。
- 没有 `ProtectSystem`/`ReadWritePaths` 的对应物 —— 因此也不再提示
  「填好配置后重跑 install 即可补上加固」，那在 procd 下是空头支票。
  能给的只有 `no_new_privs`。

### P4.3.3 运行身份的三条规则（借鉴 xray 安装脚本）

1. **`--run-as <user>`** 显式指定；用户级服务与 procd 下给了即报错（不静默忽略旗子）。
2. **重装保留既有身份**：Linux 上优先问 `systemctl show -p User --value` 要**合并后**的
   生效值——drop-in 里的 `User=` 只 grep 主 unit 是看不到的（xray 的做法会漏）。
3. **写入前校验用户存在**，避免装得上、起不来、报的还是跟同步无关的错。
4. **自动把配置 chown 给运行用户**（权限仍 0600）：配置属主不对，服务读不到就起不来。
   改属主而非放宽权限，密钥暴露面一点不扩大。

### P4.3.4 服务文件里必须写稳定路径

`os.Executable()` 的结果**不做 `EvalSymlinks`**。包管理器给的正是一个稳定软链
（brew cask：`/opt/homebrew/bin/local-mirror` → `Caskroom/<版本>/local-mirror`），
解析后会把**版本化路径**烤进服务文件，下次 `brew upgrade` 删掉旧 Caskroom 目录，
服务就再也起不来。软链本身才是该写进去的长期有效路径（v2.2.1 修复）。

### P4.4 Windows 的诚实范围

Windows 服务要接 SCM（Service Control Manager），Go 得引 `golang.org/x/sys/windows/svc`
并改造进程生命周期——**这是独立的一块工作量，不应塞进本期**。

本期 Windows 的处理：`service install` **仍然创建配置目录与空白配置**，
但服务注册部分打印手工指引（计划任务或 `sc.exe` 命令），并明确说明「原生服务支持见后续版本」。
**宁可少做并说清楚，也不要生成一个装上去跑不起来的东西。**

---

## P5. 打包投递 —— **已推翻，改为纯二进制分发**

> **2026-08-03 决策反转**：deb/rpm 已从 `.goreleaser.yaml` 移除。
> 下面保留原设计与推翻理由，供日后有人再提起时不必重吵一遍。

### P5.1 二进制名不该带版本号（结论仍然有效）

`.goreleaser.yaml` 产出的二进制**本来就叫 `local-mirror`**，版本只出现在压缩包名上。
生产上那个 `local-mirror-2.0.1` 是当初手工 scp 部署时自己加的，不是打包产物。
结论：这个问题不需要"解决"，只需要停止手工搬运。**软链方案作废**。

### P5.2 为什么放弃 deb/rpm

原设计让 deb/rpm 投递可用的 unit 与空白配置，并已实现验证过。推翻理由：

**① 我们没有 apt/yum 源。** 没有源的 `.deb` 只是"带额外步骤的 tar 包"——
照样得手动下载，没有 `apt update && apt upgrade`，没有依赖解析（静态二进制本就无依赖）。
**包管理最大的价值（仓库化的生命周期管理）一条都没拿到。**

**② 残余价值已被别的东西覆盖：**

| deb 的残余价值 | 现在谁提供 |
|---|---|
| 升级不覆盖配置 | `service install` 本就绝不覆盖已有配置 |
| 干净卸载 | `service uninstall` |
| 服务文件放对位置 | `service install`（还跨三种 init，deb 只管 systemd） |

**③ 覆盖面反而更窄。** 二进制是 `CGO_ENABLED=0` 的纯静态程序：

| | deb + rpm | 静态二进制 + install.sh |
|---|---|---|
| Debian/Ubuntu、RHEL/Fedora | ✅ | ✅ |
| Alpine（musl）、Arch、NixOS | ❌ | ✅ |
| **OpenWrt**（musl + procd） | ❌ | ✅ 已实测 |
| macOS / Windows | ❌ | ✅ |

**用更少的机器换更多的覆盖。**

### P5.3 取而代之的分发形态

```
install.sh（识别 OS/架构、校验 checksum、放进 PATH）
   └─ WITH_SERVICE=1 时代理调用 → local-mirror service install
```

**刻意让 install.sh 代理而非重写**：服务文件生成逻辑覆盖三种 init、有 20+ 单测守着
（XML 转义、用户校验、重装保留身份、属主 chown、关键路径跳过加固）。
用 POSIX sh 重写一遍会脆得多且测不了。各干一件事：
install.sh 负责"把正确架构的二进制放到正确位置"，`service install` 负责服务生命周期。

**同时保证了**：brew / scoop / 手动下压缩包 / `go build` 自己编的用户，
一样能用 `service install`——服务能力不依附于安装脚本。

### P5.4 顺带发现的 busybox 兼容性问题

install.sh 原先用 `install -m 755` 落二进制，**busybox 没有 `install(1)`**（OpenWrt 实测）。
改为 `rm -f` + `cp` + `chmod`——顺带同时绕开两个坑：Linux 上覆盖运行中的可执行文件会
ETXTBSY「文本文件忙」，Apple Silicon 上则会让新副本因签名失效被内核 SIGKILL。
换个 inode 两者都不存在。

## 迁移：现有生产两端

**debian（汇，systemd）**

1. 装 deb → `/usr/bin/local-mirror`
2. 把现有 unit 里的旗子翻译进 `/etc/local-mirror/config.yml`：

   ```yaml
   tasks:
     - name: mac-project
       receive: true
       listen: true
       path: /home/xwvike/mirrors/Project
       allow_delete: true
       loglevel: warn
       secret: <现 EnvironmentFile 里的值>
   ```
3. unit 换成 `ExecStart=/usr/bin/local-mirror --config /etc/local-mirror/config.yml`；
   删掉陈旧的 `After=…wg-quick@wg0.service`（拓扑早已改为公网 v6，
   debian 是监听方，不需要 WG，而该服务确实 enabled，会白等）
4. 删 `~/.config/local-mirror/env` 与 `EnvironmentFile=`
5. 旧的 `~/.local/bin/local-mirror-*` 全部清掉

**mac（源，launchd）**：同理，`service install --user` 生成 plist，
那串很长的 `-i node_modules,dist,…` 变成配置里的 `ignore:` 列表（YAML 数组，
天然没有「逗号分隔字符串带空格就炸」的问题）。

**兼容性**：P1~P5 全部是**加法**——既有的纯 CLI 旗子用法、既有的 `--config <path>` 显式用法
全部不变，协议不动，**不是 flag-day**，两端可各自升级。

---

## 已拍板

| # | 议题 | 结论 |
|---|---|---|
| 1 | 版本号 | **2.1.0**。纯加法、协议不变，不够格 3.0 |
| 2 | `service install` 默认 `--now` | **不带**。配置还是空的，起来必然失败，由用户填完再手动 start |
| 3 | deb 安装时 `enable` 服务 | **不**。同 2 |
| 4 | 配置自动发现 | **整个砍掉**，只保留显式 `--config`（见 P1.5） |
| 5 | Windows 原生服务 | **排除在本期外**，单独立项（见 P4.4） |
| 6 | macOS 的 `--system`（LaunchDaemon） | **支持但不默认**，默认 `--user`（见 P4.3） |
| 7 | `LOCAL_MIRROR_SECRET` | **直接移除**，不走废弃周期；父子通道改走 stdin（见 P2.3） |
| 8 | `ReadWritePaths` 生成粒度 | 逐 task `path` 列出；根在关键路径上则**整个跳过加固**并提示（见 P4.3） |

**设计决策已全部落定，可进入实现。**

## 实现顺序

| 阶段 | 内容 | 可独立验证 |
|---|---|---|
| **1** | P2.3 密钥走 stdin + 移除 `LOCAL_MIRROR_SECRET` | 多任务 e2e：`ps`/`environ` 均无密钥 |
| **2** | P3 单任务不 fork | 单任务配置启动后进程数 = 1 |
| **3** | P1 配置路径常量 + R1/R4 校验 | 单测：缺失配置 exit 2；配置在同步根内 exit 2 |
| **4** | P4 `service` 子命令（Linux + macOS） | 真机装/卸/查一轮 |
| **5** | P5 goreleaser 投递 unit + 空白配置 | `dpkg -i` 后三步跑通 |

阶段 1~3 都是**纯代码 + 单测可验证**，不碰生产；阶段 4~5 涉及真机与发布物，
建议在阶段 1~3 全绿并发过一个 RC 之后再做。
