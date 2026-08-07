# local-mirror 源码审计报告

> 作者：Codex（OpenAI）  
> 审计日期：2026-08-07  
> 审计对象：`main` 分支，提交 `3786c3f`（tag `v2.2.3`）  
> 审计方法：先脱离 README/设计文档，仅依据源码还原功能并检查实现；完成代码审计后，再回看文档核对承诺偏差。

---

> ## 📌 Claude 复核说明（2026-08-07，基于 `v2.2.3` 同一提交）
>
> 已对照现网源码逐条校对全部 16 条发现。**结论：16 条全部成立**，无一误报——
> 这是一份质量很高的审计。校对痕迹以 `> **✅ Claude 校对**` 区块直接嵌在每条发现下方，
> 总览表新增「校对」列给出一句话结论。
>
> **两点需要在读严重度时记住：**
>
> 1. **行号有系统性漂移**：Codex 给的部分 `file:line` 对不上（例如 `FindDifferences`
>    被标成 `internal/reality.go:45`，实际在 `internal/diffQueue.go:20`；`reality.go`
>    真身只有 40 行的 file-server 引导）。**实质逻辑全部属实**，仅定位需按函数名重新锚定，
>    每条校对区块已给出订正后的真实位置。
>
> 2. **严重度要套现网部署再读**：本机拓扑是 **Mac→debian 单向、单一可信对端、
>    全程 PSK（指纹 `63ad56fd`）+ WireGuard**。因此 SEC-01 这类「默认明文/监听全网卡」
>    在**当前部署里已被 PSK+WG 缓解**；但它们对「按公网无密钥示例直接暴露」的通用形态
>    仍是真 P0。**唯一不受加密保护的是 SEC-02**——它是「已通过握手的对端」越权读取，
>    PSK 挡不住，是本报告里最该优先动手的一条。修复排序见 §8，但请以此为准重排。

## 1. 总体结论

这个项目已经不是简单的目录复制脚本，而是一套功能比较完整的单向目录镜像程序：包含源/汇双角色、四种连接方向、局域网发现、中继、断点续传、可选 Noise 加密、多任务监督、系统服务安装以及运行状态观测。

工程表面质量不错：仓库干净、格式统一、跨平台文件拆分清楚，也有相当数量的回归测试。但是，当前实现仍存在数个需要优先处理的安全和一致性缺陷：

- 默认明文且无身份认证，与“可安全跨公网运行”的定位冲突。
- 文件服务接口绕过忽略规则和共享目录树，可直接请求未公开文件。
- 两端都没有可靠阻止“同步根内符号链接父目录”造成的根目录逃逸。
- 汇端运行期间的磁盘漂移不会被所谓“全量扫描”发现。
- 默认不删除的语义会被重命名优化绕过。
- 文件与目录同名互换无法稳定收敛。
- watcher 注册失败时，目录可能既不实时监听也不进入轮询。

结论：**当前版本适合继续开发和受控环境试用，但不建议在未完成 P0 修复前直接暴露到不可信网络或公网。**

## 2. 从代码识别出的实际功能

### 2.1 核心同步链路

```text
源端磁盘
  -> BLAKE3 哈希 + bbolt 目录树
  -> fsnotify 热目录监听 / 冷目录轮询
  -> changed_dirs 长轮询通知
  -> TCP 分块文件传输
  -> 汇端分片续传和整文件哈希校验
  -> 原子替换目标文件并更新汇端 bbolt 缓存

relay = 上游汇端引擎 + 下游源端服务，共享同一同步目录和数据库
```

### 2.2 已实现的功能面

- `--send` 表示本端是源，`--receive` 表示本端是汇；两者同时给出表示中继。
- 数据方向与 TCP 建连方向解耦，支持源监听、源拨出、汇拨出、汇监听。
- IPv4/IPv6 双栈监听、域名连接以及重连时重新解析域名。
- UDP 局域网发现、实例别名、端口段扫描和多源交互选择。
- 自定义 v3 二进制协议，包含版本协商、角色校验、结构化错误和消息体上限。
- 可选 Noise NNpsk0 加密、密钥文件、密钥生成、指纹显示和 stdin 密钥传递。
- 文件树持久化、BLAKE3 内容比较、变更长轮询和低频全量对账。
- 大文件分片传输、断点续传、完整性校验和磁盘剩余空间预检。
- 默认增量同步、可选删除、关键路径保护和覆盖前快照。
- 文件 mtime 对齐和同目录文件重命名优化。
- YAML 多任务、子进程监督、退避重启和信号转发。
- systemd、launchd、procd 服务文件生成与安装/卸载/状态查询。
- `--status`、`--heat`、日志轮转和本机运行实例发现。

## 3. 问题总览

| 编号 | 严重度 | 类型 | 结论 | 校对 |
|---|---|---|---|---|
| SEC-01 | P0 | 安全设计 | 默认明文、无认证并监听所有网卡 | ✅ 确认；现网 PSK+WG 已缓解，对无密钥公网形态仍是真 P0 |
| SEC-02 | P0 | 越权读取 | 文件请求绕过忽略规则和共享树授权 | ✅ 确认；**加密挡不住，最高优先** |
| SEC-03 | P0 | 路径逃逸 | 源端可经符号链接父目录读取根外文件 | ✅ 确认；依赖 SEC-02 可达，随其一并修复 |
| SEC-04 | P0 | 路径逃逸 | 汇端可经符号链接父目录写入/删除根外内容 | ✅ 确认；需汇端已存在软链为前置 |
| COR-01 | P1 | 一致性 | 汇端“全量扫描”不检查真实磁盘 | ✅ 确认 |
| COR-02 | P1 | 删除语义 | 重命名优化绕过 `--allow-delete` | ✅ 确认 |
| COR-03 | P1 | 类型冲突 | 文件与目录互换无法收敛 | ✅ 确认 |
| COR-04 | P1 | 变更检测 | watcher 注册失败的目录从两级监控中丢失 | ✅ 确认 |
| CFG-01 | P1/P2 | 参数校验 | `filebuffersize=0` 可造成发送循环空转 | ✅ 确认；仅裸 CLI，YAML/监督路径已兜底 |
| CFG-02 | P2 | 配置安全 | YAML 未拒绝未知字段，敏感配置拼错可静默降级 | ✅ 确认 |
| PERF-01 | P2 | 复杂度 | 超大目录每一页都重新加载并排序完整目录 | ✅ 确认 |
| PERF-02 | P2 | 复杂度 | changed_dirs 使用切片去重，批量变化时为 O(N^2) | ✅ 确认 |
| PERF-03 | P2 | 轮询效率 | 忽略项/特殊文件可让 tier2 长期保持高频扫描 | ✅ 确认 |
| DB-01 | P2 | 状态准确性 | 批量删除先计数后去重，元数据计数可能错误 | ✅ 确认；仅影响 --status 计数 |
| SVC-01 | P2 | 服务配置 | systemd/procd 路径未正确引用或转义 | ✅ 确认；仅含空格/特殊字符路径，launchd 已转义 |
| ENG-01 | P2 | CI | PR/main CI 不运行测试 | ✅ 确认 |

## 4. 详细发现

### SEC-01：默认明文、无认证并监听所有网卡

**严重度：P0**

没有显式 `-k` 且同步根中没有密钥文件时，程序保留明文模式：

- [`cmd/local-mirror/main.go:258`](../cmd/local-mirror/main.go#L258)
- [`internal/network/client.go:80`](../internal/network/client.go#L80)
- [`internal/network/client.go:105`](../internal/network/client.go#L105)

监听端同时绑定 `0.0.0.0` 和 `[::]`：

- [`internal/network/server.go:78`](../internal/network/server.go#L78)

应用层握手只校验协议版本和数据方向，不提供独立的身份认证。由此产生两种暴露面：

1. 源端监听时，任何网络可达客户端都可以扮演汇端并请求文件。
2. 汇端监听时，任何网络可达客户端都可以扮演源端，控制汇端随后获取的目录树和文件内容。

这不是隐藏行为，README 将加密描述为“可选”；但项目同时提供公网监听示例，而且示例没有密钥，因此它是一个严重的安全设计风险。

**建议：**

- 公网/非回环监听默认要求密钥，明文必须显式 `--no-encrypt` 确认。
- 或至少默认仅监听回环/局域网地址，并新增明确的 `--bind`/`--public` 策略。
- 在身份认证完成前不要进入目录树和文件服务逻辑。
- 对明文 LAN 发现增加醒目的启动警告，而不是只在 banner 中显示 `off`。

> **✅ Claude 校对（SEC-01）— 确认，现网已缓解但通用形态仍是真 P0**
>
> 代码属实：`main.go` 无 `-k` 且同步根无密钥文件时保持明文（真实位置
> `cmd/local-mirror/main.go:256-266`，非报告标的 258 那行是注释）；监听端
> `internal/network/server.go:78` 的 `ListenAvailable` 确实同时绑 `0.0.0.0` 与 `[::]`，
> 无 `--bind` 选项。握手只校验协议版本+数据方向，无独立身份认证。
>
> **部署校准**：本机现网全程 PSK（`63ad56fd`）+ WG，270 次公网扫描零握手成功，
> 这条在**当前部署里已被缓解**。真正的风险面是「按 README 无密钥公网示例直接跑」。
> **动手建议**：非回环监听默认要求密钥、明文须显式 `--no-encrypt`；README 公网示例补密钥。
> 属于「加固通用形态」，优先级低于 SEC-02。

### SEC-02：文件请求绕过忽略规则和共享树授权

**严重度：P0**

文件服务处理器直接接受客户端提供的路径：

- [`internal/network/server.go:524`](../internal/network/server.go#L524)

处理器只做词法根目录检查和最终路径的 `Lstat`，没有：

- 调用 `utils.IsIgnored`；
- 检查目标是否存在于 bbolt 目录树；
- 检查目标节点是否为当前协议会话允许访问的普通文件。

因此，自定义客户端可以跳过目录树枚举，直接请求：

- `.git/config`；
- 用户通过 `-i` 忽略的密钥、配置或构建产物；
- `.local-mirror/cache.db`、日志和其他内部状态文件；
- 其他位于同步根中但不在公开树里的普通文件。

这与 README 中“服务端命中即不扫描不提供”的承诺直接矛盾。

**建议：**

- 文件请求首先规范化为协议路径，再拒绝所有忽略项。
- 必须查询树索引，只有树中存在、哈希非空、类型为普通文件的节点才允许读取。
- 为 `.local-mirror` 增加独立硬拒绝，不依赖可配置忽略列表。
- 添加直接构造 `FileRequest` 请求 `.git/config`、`.local-mirror/*` 和自定义忽略项的协议测试。

> **✅ Claude 校对（SEC-02）— 确认，本报告最该优先动手的一条**
>
> 逐行核实 `handleFileRequest`（`internal/network/server.go:524-615`）：只做
> ① `filepath.Rel` 词法根检查（537）② 末段 `Lstat` 拒符号链接（542）③ `Stat`+哈希+`Open`。
> **确实从不调用 `utils.IsIgnored`，也从不查 bbolt 目录树**。因此已握手的对端可构造
> `FileRequest` 直取 `.git/config`、`.local-mirror/cache.db`、`-i` 忽略的任何普通文件——
> 与 README「命中忽略即不提供」直接矛盾（§6.1）。
>
> **为何最优先**：这不是「网络暴露」类问题，是**已通过握手的对端越权**——PSK/WG 一概挡不住。
> 威胁模型里对端即使可信，一旦对端被攻陷或换成恶意实现，就能把源端同步根里所有普通文件
> （含被忽略的密钥/配置）整个抽走。**修复**：文件请求先规范化，强制「树中存在 + 哈希非空 +
> 普通文件 + 非忽略」四条全过才放行，`.local-mirror` 独立硬拒。此修复同时基本关掉 SEC-03。

### SEC-03：源端可经符号链接父目录读取根外文件

**严重度：P0**

源端只检查最终路径本身是否是符号链接：

- [`internal/network/server.go:535`](../internal/network/server.go#L535)
- [`internal/network/server.go:542`](../internal/network/server.go#L542)

例如：

```text
/srv/source/outside -> /etc
```

客户端请求 `outside/passwd` 时：

- 词法路径仍是 `/srv/source/outside/passwd`，所以 `filepath.Rel` 判定在根内；
- `Lstat` 只作用于最终的 `passwd`，中间的 `outside` 会被内核解析为符号链接；
- 随后的 `Stat`、哈希和 `Open` 实际读取 `/etc/passwd`。

建树时跳过符号链接不能防住这个问题，因为 SEC-02 允许客户端直接请求任意路径。

**建议：**

- 对所有现存路径组件逐级执行 no-follow 校验。
- Linux 优先考虑 `openat2(RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS)`；其他平台需要等价的目录句柄相对操作或严谨的逐组件策略。
- 不要仅依赖 `EvalSymlinks` 后再普通 `Open`，两步之间存在 TOCTOU 窗口。

> **✅ Claude 校对（SEC-03）— 确认，与 SEC-02 耦合**
>
> 属实：`server.go:542` 的 `os.Lstat(fullPath)` 只判**末段**是否软链，中间目录组件
> （如 `outside -> /etc`）会被后续 `os.Stat`/`os.Open`（545/567）正常解引用，读到 `/etc/passwd`。
> 建树跳过软链不能防，因为 SEC-02 允许绕过树枚举直接点名任意路径。
>
> **关键耦合**：本条的可达性完全依赖 SEC-02（能直接请求任意路径）。一旦 SEC-02 改为
> 「必须树中存在」，软链父目录下的目标本就不在树里，SEC-03 随之基本关闭。真正要额外补的是
> TOCTOU 收口——`openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)`（Linux）或逐组件 no-follow，
> 别用「`EvalSymlinks` 后再普通 `Open`」的两步式。

### SEC-04：汇端可经符号链接父目录写入或删除根外内容

**严重度：P0**

`SafeJoin` 的注释和实现都明确说明它只做词法清洗：

- [`internal/safety/paths.go:13`](../internal/safety/paths.go#L13)
- [`internal/safety/paths.go:20`](../internal/safety/paths.go#L20)

后续文件系统操作会正常解析中间符号链接：

- 目录创建：[`internal/mirror.go:142`](../internal/mirror.go#L142)
- 删除：[`internal/mirror.go:109`](../internal/mirror.go#L109)
- 文件最终替换：[`internal/network/client.go:539`](../internal/network/client.go#L539)
- 重命名优化：[`internal/mirror.go:437`](../internal/mirror.go#L437)

例如汇端已有：

```text
/srv/sink/outside -> /tmp/other
```

恶意或被劫持的源端下发 `outside/payload` 后，最终 `os.Rename` 可以把文件写入 `/tmp/other/payload`。`--allow-delete` 开启时，对符号链接父目录下的路径执行 `RemoveAll` 也可能影响根外内容。

**建议：**

- 将“路径合法性”和“安全执行文件操作”统一到一个 filesystem boundary 层。
- 创建、替换、删除、改时间和本地重命名全部必须复用同一套 no-follow 目录句柄逻辑。
- 增加符号链接父目录的写入、删除、目录创建和重命名测试。

> **✅ Claude 校对（SEC-04）— 确认，需汇端已存在软链为前置**
>
> 属实：`SafeJoin`（`internal/safety/paths.go:20`）注释与实现都明确「仅词法清洗（Clean），
> 不依赖磁盘状态」，中间软链由后续 `MkdirAll`/`RemoveAll`/`os.Rename` 正常解引用。
> 逃逸路径成立。
>
> **前置条件要说清**：需要**汇端同步根内已存在一个指向外部的软链**。而 local-mirror
> 自己既不创建软链、源端也不下发软链（建树跳过），所以这个软链必须由**外部进程**预先放进汇端根。
> 因此实际触发门槛高于纯 P0，属「纵深防御缺口」。**修复**：把「路径合法性」与「安全落盘」
> 统一到一个 no-follow 目录句柄边界层，create/replace/delete/rename/chtimes 全复用它——
> 与 SEC-03 同一套机制，一次做完。

### COR-01：汇端“全量扫描”不检查真实磁盘

**严重度：P1**

程序只在启动时执行一次 `BuildFileTree`：

- [`internal/app.go:14`](../internal/app.go#L14)

之后的 `fullScan` 会递归拉取源端目录树：

- [`internal/mirror.go:657`](../internal/mirror.go#L657)

但差异比较的“本地树”来自 bbolt，而不是当前磁盘：

- [`internal/reality.go:77`](../internal/reality.go#L77)

汇端和中继也没有 fsnotify watcher。由此导致运行期间的本地漂移无法被发现：

- 本地文件被删除，不会重新下载；
- 本地文件内容被篡改，不会恢复；
- 本地新增文件不进入数据库，即使 `--allow-delete` 开启也不会删除；
- 中继目录被外部程序改动时，下游可能继续看到缓存中的旧状态。

只有重启进程、重新构建本地树后，部分漂移才会被修复。

**建议：**

- 真正的周期性 reconciliation 必须以磁盘现实为准，而不是只读缓存。
- 可以在全量扫描前对相关目录做轻量校准，或为汇端增加专门的本地变更检测。
- 若仍保留缓存快速路径，应至少对 size、mtime、类型和存在性重新 `Lstat`；高完整性模式应重新哈希。
- 增加长驻进程端到端测试：初次同步后在汇端删除、修改、新增文件，再等待全量扫描验证恢复。

> **✅ Claude 校对（COR-01）— 确认**
>
> 属实：`BuildFileTree` 只在启动跑一次（`internal/app.go` 的启动路径）；`fullScan`
> （`internal/mirror.go:657`）拉源端树后走 `Diff`（`internal/diffQueue.go:78`）比对的「本地树」
> 来自 `tree.GetDirContents`（读 bbolt），**不 stat 磁盘**；汇端/中继无 fsnotify watcher。
> 故运行期汇端本地漂移（被删/被改/新增）在重启前不会被发现或修复。
> （报告标的 `internal/reality.go:77` 为行号漂移，真身在 diffQueue.go。）
>
> **对本机的意义**：汇端是纯备份、无人本地编辑，日常影响小；但「有人手滑动了备份目录」
> 这类场景确实要重启才自愈。**修复**：周期对账以磁盘为准，或至少对相关目录重新
> `Lstat`（size/mtime/类型/存在性），高完整性模式重哈希。

### COR-02：重命名优化绕过 `--allow-delete`

**严重度：P1**

目录差异会无条件进入重命名检测：

- [`internal/mirror.go:309`](../internal/mirror.go#L309)

匹配到相同哈希的 `delete + create` 后直接执行 `os.Rename`：

- [`internal/mirror.go:392`](../internal/mirror.go#L392)
- [`internal/mirror.go:451`](../internal/mirror.go#L451)

而 `--allow-delete` 的判断只存在于后来处理剩余 `delete` diff 的路径：

- [`internal/mirror.go:99`](../internal/mirror.go#L99)

结果是默认增量模式下，源端把 `a` 重命名为 `b`，汇端的 `a` 仍会消失。按照当前文档语义，默认模式应该保留 `a`，并另外创建 `b`。

此外，重命名优化信任数据库保存的旧哈希，不重新验证本地旧文件。若汇端文件已经发生 COR-01 所述的本地漂移，错误内容会被移动到新路径，并被登记成源端哈希。

**建议：**

- `--allow-delete=false` 时完全禁用会移除旧路径的 rename 优化。
- `--allow-delete=true` 时，执行 rename 前重新校验磁盘旧文件类型、位置和完整哈希。
- 增加默认增量与忠实镜像两种模式的重命名测试。

> **✅ Claude 校对（COR-02）— 确认**
>
> 属实：`drainNextLevel` 里 `detectRenames(diffs)` 在 `internal/mirror.go:310`
> **无条件先跑**，`applyRename`（`mirror.go:437-465`）直接 `os.Rename` 抹掉旧路径；而
> `--allow-delete` 的判断只在 `processDiffItem` 的 delete 分支（`mirror.go:105`）。故默认
> 增量模式下源端把 `a` 改名 `b`，汇端 `a` 仍消失——违反「默认只同步不删」（§6.3）。
> 且 `detectRenames` 信任 DB 旧哈希、不重验本地旧文件，叠加 COR-01 漂移会把错内容搬到新路径。
>
> **修复**：`--allow-delete=false` 时彻底禁用「会移除旧路径」的 rename 优化（退化为
> create 新文件、保留旧文件）；`=true` 时 rename 前重验本地旧文件类型+完整哈希。

### COR-03：文件与目录互换无法收敛

**严重度：P1**

`FindDifferences` 对同路径节点只比较大小和哈希，没有比较 `IsDir`：

- [`internal/reality.go:45`](../internal/reality.go#L45)

后果：

- 若大小碰巧相同且哈希条件不成立，类型变化可能完全不产生 diff；
- 源端目录变文件时，下载完成后的 `os.Rename` 无法覆盖现有目录；
- 源端文件变目录时，`MkdirAll` 会因同名文件存在而失败；
- 即使 `--allow-delete` 开启，也不会先删除旧类型再创建新类型。

**建议：**

- 类型变化必须是独立 diff 类型，不能当作普通 `modify`。
- 忠实镜像模式下执行“安全删除旧类型 -> 创建新类型”。
- 默认增量模式下要明确策略：拒绝并告警，或要求用户显式允许替换；不能无限重试。
- 增加 file-to-dir、dir-to-file、空目录和非空目录测试。

> **✅ Claude 校对（COR-03）— 确认**
>
> 属实：`FindDifferences`（`internal/diffQueue.go:46`，非报告标的 reality.go:45）比对同路径
> 节点只看 `Size` 与 `Hash`，**不比 `IsDir`**。故同大小的文件↔目录互换可能完全不产生 diff；
> 即便产生，`os.Rename` 覆盖不了同名目录、`MkdirAll` 撞同名文件失败，也不会先删旧类型。
>
> **修复**：把「类型变化」提升为独立 diff 类型；忠实镜像模式「安全删旧类型→建新类型」，
> 默认增量模式明确策略（拒绝并告警，不能无限重试）。补 file→dir / dir→file 测试。

### COR-04：watcher 注册失败时目录从两级监控中丢失

**严重度：P1**

`performScan` 为 tier1 目录调用 `Watcher.Add`；失败后直接 `continue`：

- [`internal/watcher/scoreWatch.go:224`](../internal/watcher/scoreWatch.go#L224)
- [`internal/watcher/scoreWatch.go:271`](../internal/watcher/scoreWatch.go#L271)

该目录既没有加入 `newTier1`，也没有加入 `newTier2`。新目录路径中的 `addHeat` 也会先把目录标成 tier1，再忽略 `Watcher.Add` 失败：

- [`internal/watcher/scoreWatch.go:437`](../internal/watcher/scoreWatch.go#L437)

当 inotify/kqueue 限额耗尽、多个任务争用 per-user 限额或单个大目录使 fd 预算超限时，目录变化可能长期不进入源端树。汇端全量扫描读取的仍是源端缓存树，因此不能补救。

**建议：**

- 任何 watcher 注册失败的存在目录都必须立即降级进入 tier2。
- 新目录 `addHeat` 只有在 `Watcher.Add` 成功后才能标记为 tier1。
- macOS 应在添加目录前预测该目录的 fd 成本，不能先越过上限再尝试添加。
- 用可注入 watcher 接口模拟 `Add` 失败并验证 tier2 fallback。

> **✅ Claude 校对（COR-04）— 确认**
>
> 属实：`performScan`（`internal/watcher/scoreWatch.go:271-274`）`Watcher.Add` 失败即
> `continue`——该目录既不进 `newTier1`（在此之后才 append），也不在 `newTier2`（只有
> `usedWatches >= limit` 的分支才落 tier2），**两级俱失**。`addHeat`（`scoreWatch.go:437-449`）
> 更是不看 `Add` 结果就无条件塞进 tier1。inotify/kqueue 限额耗尽或多任务争用时，目录变化
> 可能长期不进源端树，汇端全量扫描读的又是源端缓存树，无从补救。
>
> **修复**：任何 `Add` 失败的现存目录立即降级 tier2；`addHeat` 仅在 `Add` 成功后才标 tier1；
> macOS 加目录前预估 fd 成本。用可注入 watcher 接口模拟 `Add` 失败验证 fallback。

### CFG-01：`filebuffersize=0` 可造成发送循环空转

**严重度：P1/P2**

CLI 接受任意 `uint64`：

- [`config/config.go:380`](../config/config.go#L380)

源端直接以它创建缓冲：

- [`internal/network/server.go:626`](../internal/network/server.go#L626)

值为零时，零长度 `Read` 可持续返回 `(0, nil)`，循环既不发送数据也不到达 EOF，形成 CPU 空转和永久卡住。超大值还可能导致：

- 巨额内存分配或 panic；
- 生成超过 `MaxBodyLength` 的消息，让接收方拒绝连接；
- 多连接场景下快速放大内存占用。

`cooldown <= 0` 同样没有被拒绝，会把低频安全网退化为反复执行。

**建议：**

- 启动时统一验证数值范围。
- `filebuffersize` 至少为 1，建议限制在合理范围，例如 4 KiB 到 4 MiB，并保证编码后小于协议消息上限。
- `cooldown` 必须为正数，或把零定义成明确的“关闭周期扫描”。

> **✅ Claude 校对（CFG-01）— 确认，但仅限裸 CLI；YAML/监督路径已兜底**
>
> 属实：CLI `-f`（`config/config.go:380`）收任意 `uint64`，无启动校验；`server.go:626`
> 直接 `make([]byte, *config.FileBufferSize)`，为 0 时 `Read` 恒返回 `(0,nil)`，
> `sendFileData` 循环既不发数据也不到 EOF——CPU 空转永久卡住。`-c 0` 同理让
> `fullScanInterval`（`mirror.go:625`）= 0，`time.Since >= 0` 恒真，每轮都全量扫描。
>
> **纠一处范围**：**YAML/监督进程路径已自带兜底**——`supervisor.go:249/252` 仅在
> `CoolDown>0` / `FileBufferSize>0` 时才透传 `-c`/`-f`，为 0 则不传旗子、子进程回落 CLI 默认值
> （`multiconfig.go:184-188` 也把 0 视作「用默认」）。所以**只有人手敲 `local-mirror -f 0`
> 才会触发**。修复仍应做：启动统一校验数值域（`filebuffersize` ≥ 1 且 < 协议消息上限；
> `cooldown` 为正或定义 0=关闭周期扫描），把兜底从「监督层」下沉到「参数解析层」。

### CFG-02：YAML 未拒绝未知字段

**严重度：P2**

配置直接使用 `yaml.Unmarshal`：

- [`config/multiconfig.go:64`](../config/multiconfig.go#L64)

未知字段会被静默忽略。例如把 `secret` 拼成 `secrect`，任务仍可能成功启动，但连接会退化成明文。类似问题还可能让 `allow_delete`、`listen`、`connect` 等关键策略与用户预期不符。

**建议：**

- 改用 `yaml.Decoder` 并启用 `KnownFields(true)`。
- 对密钥、连接方向、数值范围和路径存在性做集中验证。
- 增加未知顶层字段、未知任务字段和拼错敏感字段的测试。

> **✅ Claude 校对（CFG-02）— 确认**
>
> 属实：`config/multiconfig.go:64` 用 `yaml.Unmarshal`，未启用 `KnownFields`。把 `secret`
> 拼成 `secrect` 会被静默忽略，任务照常启动但退化明文；`allow_delete`/`listen`/`connect`
> 拼错同理静默偏离预期。（现有校验只覆盖空 tasks、重复路径/名，管不到未知字段。）
>
> **修复**：改 `yaml.NewDecoder(...).KnownFields(true)`，并对密钥、连接方向、数值域、
> 路径存在性做集中校验。补未知顶层/任务字段、拼错敏感字段的测试。

### PERF-01：超大目录分页重复加载和排序完整目录

**严重度：P2**

每一个目录页请求都会：

1. 从 DB 重新加载完整目录内容；
2. 对完整 slice 排序；
3. 再根据游标截取当前页。

相关代码：

- [`internal/network/server.go:469`](../internal/network/server.go#L469)
- [`internal/network/server.go:494`](../internal/network/server.go#L494)

若目录有 N 个条目、每页 P 个条目，客户端会请求约 N/P 页，服务端会重复约 N/P 次全量反序列化和排序。百万级单目录会出现明显 CPU、内存和 DB 读取放大。

**建议：**

- 让数据库索引天然按路径顺序分页，避免每页重新构造完整 slice。
- 或为一次会话建立有过期时间的稳定目录快照和游标。

> **✅ Claude 校对（PERF-01）— 确认**
>
> 属实：`handleTreeRequest`（`server.go:494`）每页都 `tree.GetDirContents` 全量反序列化，
> 再 `pageTreeEntries`（`server.go:469-470`）对**完整 slice** `sort.Slice`，然后按游标截取。
> N 条目/页 P 条 → 约 N/P 页 → 全量反序列化+排序重复 N/P 次。百万级单目录 CPU/内存/DB 读放大明显。
> 代码注释已承认页间条目增删由变更推送+全量扫描兜底。
>
> **修复**：让 DB 按路径序天然分页（避免每页重建完整 slice），或建带过期的会话级稳定快照+游标。

### PERF-02：changed_dirs 去重为 O(N^2)

**严重度：P2**

`AddRecentChangedDir` 使用 `slices.Contains` 对 slice 去重：

- [`internal/tree/buildFileTree.go:51`](../internal/tree/buildFileTree.go#L51)

两秒批处理窗口内若出现大量不同目录，追加第 N 项要线性扫描前 N-1 项，总成本为 O(N^2)。大规模代码生成、解压或批量移动时会明显放大 CPU 和锁持有时间。

**建议：** 使用 `map[string]struct{}` 作为批次去重集合，落库时再转 slice。

> **✅ Claude 校对（PERF-02）— 确认**
>
> 属实：`AddRecentChangedDir`（`internal/tree/buildFileTree.go:55`）用 `slices.Contains`
> 对 slice 去重，2 秒批窗内大量不同目录追加第 N 项要线扫前 N-1 项，总 O(N²)。大规模代码生成/
> 解压/批量移动时放大 CPU 与锁持有。
>
> **修复**：批内用 `map[string]struct{}` 去重，落库时再转 slice（低风险小改）。

### PERF-03：忽略项和特殊文件可使 tier2 长期高频扫描

**严重度：P2**

`hasDirectoryChanged` 先把磁盘条目加入 `newNodes`，发现数据库中不存在就调用 `eventFilter` 并将 `changed=true`：

- [`internal/watcher/scoreWatch.go:381`](../internal/watcher/scoreWatch.go#L381)

但 `eventFilter` 随后会丢弃忽略项、符号链接和非普通文件。这些条目始终不会进入数据库，于是下一轮仍会被视为“新增变化”，tier2 的自适应退避会反复重置到 30 秒。

**建议：** 在设置 `changed=true` 前，先应用与建树一致的忽略、符号链接和普通文件过滤规则。

> **✅ Claude 校对（PERF-03）— 确认，且 v2.2.3 的修复没覆盖到这里**
>
> 属实：`hasDirectoryChanged`（`internal/watcher/scoreWatch.go:386-391`）对 DB 中不存在的
> 磁盘条目**先无条件 `changed=true`**，`eventFilter` 才在其后丢弃忽略项/软链/非普通文件。
> 被丢的条目永不进 DB，下一轮仍算「新增变化」，tier2 自适应退避被反复重置回 30 秒。
>
> **要点**：`v2.2.3` 我在 `eventFilter`/建树侧加的「跳过非普通文件」只堵住了「进不可读表 +
> FIFO 挂死」，**没堵这条 tier2 空转**——因为空转的根因是 `changed=true` 早于 `eventFilter`。
> **修复**：在置 `changed=true` 前先套用与建树一致的忽略/软链/普通文件过滤。

### DB-01：批量删除的元数据计数可能错误

**严重度：P2**

`DeleteNodes` 先对每个输入路径递归统计目录和文件数量，然后才对实际待删除 ID 去重：

- [`internal/tree/treeDB.go:433`](../internal/tree/treeDB.go#L433)
- [`internal/tree/treeDB.go:442`](../internal/tree/treeDB.go#L442)

若同一批次同时包含父目录和其子节点，子树会被重复计数，但只删除一次。结果可能是：

- `dir_count`/`file_count` 被减得过多；
- 若重复计数超过旧值，更新逻辑直接跳过，计数保持陈旧。

这主要影响状态展示和监控，不直接破坏文件内容。

**建议：** 对节点 ID 去重完成后，再依据唯一节点集合计算类型计数。

> **✅ Claude 校对（DB-01）— 确认，仅影响状态计数**
>
> 属实：`DeleteNodes`（`internal/tree/treeDB.go:433-435`）**先**对每个输入路径递归累加
> `totalDirCount/totalFileCount`，**后**才对实际待删 ID 去重（`:443-446`）。同批次若同含父目录
> 与其子节点，子树被重复计数但只删一次 → `dir_count`/`file_count` 减过头；若过头量超旧值，
> 更新逻辑跳过、计数陈旧。只影响 `--status`/监控展示，不损文件内容。
>
> **修复**：ID 去重完成后，再按唯一节点集合算类型计数。

### SVC-01：systemd/procd 路径未正确引用或转义

**严重度：P2**

systemd 的 `ExecStart` 和 `ReadWritePaths` 直接拼接路径：

- [`cmd/local-mirror/service.go:57`](../cmd/local-mirror/service.go#L57)

procd init 脚本也把二进制和配置路径直接插入 shell 命令：

- [`cmd/local-mirror/service.go:149`](../cmd/local-mirror/service.go#L149)

路径中包含空格、引号、分号、反斜杠或 systemd 特殊字符时，服务文件可能无法启动；procd 路径还存在 shell 语义注入风险。调用者通常已有安装权限，所以它未必构成额外提权，但仍会产生错误或危险的服务配置。

**建议：**

- systemd 使用符合 unit 语法的参数引用函数逐项编码。
- procd 使用严格的 POSIX shell 单引号编码，不能直接插值。
- 增加包含空格、单引号、分号和 `%` 的路径测试。

> **✅ Claude 校对（SVC-01）— 确认，低危；launchd 已豁免**
>
> 属实：`systemdUnitText`（`cmd/local-mirror/service.go:69`）`ExecStart=%s --config %s`
> 直插，`ReadWritePaths=%s`（`:75`）用空格 `Join`——路径含空格会被 systemd 拆成多路径/多参数；
> `procdInitScript`（`:158`）把二进制/配置直插进 sh 脚本，含 `;`/`$()` 有 shell 注入面。
> **补一句报告没点破的**：`launchdPlistText`（`:88-114`）已逐项 `xml.EscapeText`，**macOS 侧不受影响**——
> 本条只打 Linux(systemd)/OpenWrt(procd)。
>
> **危害有限**：路径来自安装位置，调用方通常已有安装权限，非提权，但含空格/引号的安装路径会产出
> 起不来或危险的服务文件。**修复**：systemd 按 unit 语法逐项引用，procd 用严格 POSIX 单引号编码，
> 补含空格/单引号/分号/`%` 的路径测试。

### ENG-01：PR/main CI 不运行测试

**严重度：P2**

主 CI 只运行格式检查、`go vet` 和构建：

- [`.github/workflows/go.yml:17`](../.github/workflows/go.yml#L17)

`go test ./...` 只出现在 release tag 工作流：

- [`.github/workflows/release.yml:26`](../.github/workflows/release.yml#L26)

这意味着测试失败的提交仍可合并进 `main`，直到发布时才暴露。

**建议：** PR/main 至少运行：

```bash
go test ./...
go test -race ./...
```

自定义协议和路径安全代码还适合增加 fuzz 测试与 Linux/Windows 交叉编译矩阵。

> **✅ Claude 校对（ENG-01）— 确认**
>
> 属实：`.github/workflows/go.yml` 只有 gofmt/`go vet`/`go build`（`:21-28`），无 `go test`；
> `go test ./...` 仅出现在 release tag 工作流。测试挂了的提交能合进 main、到发版才暴露。
> 与 [[release-process]] 记的「release workflow 不跑 gofmt、main 红也能发」是同一类 CI 盲区。
>
> **修复**：PR/main 至少加 `go test ./...` 与 `go test -race ./...`；协议与路径安全代码适合
> 补 fuzz + Linux/Windows 交叉编译矩阵（后者正好接上 [[release-process]] 里踩过的 Windows 编译失败）。

## 5. 其他实现风险与工程债务

### 5.1 错误传播被弱化

`executeTaskWithClient` 遇到非连接类错误时只记录日志，随后仍返回 `nil`：

- [`internal/mirror.go:75`](../internal/mirror.go#L75)

这可能让初始全量扫描或数据库错误在上层被当成成功。建议明确区分“可跳过单项错误”和“本轮任务失败”，不要在通用包装器中吞掉后者。

### 5.2 文件哈希带来多次完整读取

一个新文件通常经历：

1. 源端建树或 watcher 阶段完整哈希；
2. 源端响应下载前再次完整哈希；
3. 汇端接收后再次完整哈希。

这是完整性和可续传设计的结果，但大文件、多客户端和机械盘场景会出现明显 I/O 放大。后续可评估文件快照、稳定 inode/mtime 校验或按会话复用可信哈希。

### 5.3 启动建树内存为 O(N)

启动校准会同时持有旧树 map、新节点 slice、路径 ID map 和已见路径集合。普通项目目录可接受，但百万级文件树会占用较多内存。可以考虑流式批处理或让 DB 承担部分集合运算。

### 5.4 大文件和多客户端缺少全局 I/O 限流

连接数限制为 256，但每个连接都可以独立触发大文件预哈希和传输。未认证监听时尤其容易被用作磁盘/CPU 放大器。除身份认证外，建议增加全局哈希与传输并发限制。

### 5.5 大量包级全局状态

项目广泛依赖：

- `config.*` 指针和运行时全局字段；
- `tree.DB`；
- `ServerListener`；
- `GlobalScoreWatch`；
- `NextLevel`；
- `lastChangeCursor`；
- 多组包级定时器、缓存和 mutex。

单实例进程中可以工作，但会增加测试隔离难度，也使未来多会话、多根目录或库化改造困难。建议逐步收敛为 `App`/`Runtime` 实例持有的显式依赖。

### 5.6 单文件职责过重

当前较大的文件包括：

- `cmd/local-mirror/main.go`：约 1330 行；
- `internal/mirror.go`：约 788 行；
- `internal/network/server.go`：约 785 行；
- `cmd/local-mirror/service.go`：约 766 行。

尤其 `main.go` 同时负责 CLI、密钥、发现、状态 TUI、banner 和启动编排。建议按 `cli`、`runtime bootstrap`、`status view`、`discovery flow` 分拆。

### 5.7 文档同时混合用户手册和历史决策记录

README 相对简洁，但 `docs/CONFIG_AND_SERVICE.md`、`docs/PUBLIC_EXPOSURE.md` 保留了大量已推翻或阶段性设计。对作者本人有价值，但外部维护者容易把旧方案误认为当前行为。建议明确标记 ADR/历史章节，或把最终状态单独提炼成稳定规范。

## 6. 文档承诺与代码偏差

> **✅ Claude 校对（§6 整节）— 四条偏差全部确认**，且都是前面缺陷的直接投影：
> §6.1「命中忽略即不提供」不成立 ← SEC-02（handleFileRequest 不查忽略/树）；
> §6.2「汇端持续保持一致」被高估 ← COR-01（漂移不进 DB）；
> §6.3「默认只同步不删」被打破 ← COR-02（rename 抹旧路径）；
> §6.4「公网示例无密钥」← SEC-01（README 示例该补密钥）。
> **修复代码时，README/设计文档这四处措辞要同步改**，否则「文档承诺」与「实际行为」继续背离。
> 注意 §5.7 也提醒：`docs/` 里混着已推翻的阶段性设计，别把旧方案误当现行为。

### 6.1 “服务端命中忽略项即不提供”不成立

README：

- [`README.zh-CN.md:125`](../README.zh-CN.md#L125)

实际文件请求未校验忽略列表，见 SEC-02。

### 6.2 “汇端保持一致”和“全量扫描安全网”被高估

README 将汇端描述为持续保持实时副本，但运行期间的本地漂移不进入数据库，见 COR-01。

### 6.3 “默认只同步不删除”被重命名优化打破

README：

- [`README.zh-CN.md:93`](../README.zh-CN.md#L93)

实际同哈希重命名会移除旧路径，见 COR-02。

### 6.4 公网示例没有体现认证是必要条件

README 给出公网监听/拨号示例，但没有密钥：

- [`README.zh-CN.md:68`](../README.zh-CN.md#L68)

而公网设计文档又写明“口令成为唯一城墙”。在 P0 修复前，至少应修正文档，不能让用户按无密钥示例直接暴露端口。

## 7. 已执行的验证

以下命令均通过：

```bash
go test ./...
go test -race ./...
go vet ./...
gofmt -l .
GOOS=linux GOARCH=amd64 go build ./...
GOOS=windows GOARCH=amd64 go build ./...
git diff --check
```

`gofmt -l .` 无输出，审计前工作树干净。

覆盖率抽查结果：

| 包 | statement coverage |
|---|---:|
| `cmd/local-mirror` | 12.4% |
| `config` | 53.8% |
| `internal` | 2.4% |
| `internal/network` | 26.4% |
| `internal/safety` | 82.5% |
| `internal/status` | 66.5% |
| `internal/tree` | 58.5% |
| `internal/watcher` | 13.6% |

仓库共有约 127 个 `Test*` 测试，但同步主链、真实网络会话和 watcher 的覆盖明显不足。

由于执行环境网络受限，以下项目没有完成：

- 在线依赖更新检查；
- Go 漏洞数据库扫描；
- 从公网下载或复核发行产物。

因此依赖供应链风险不在本报告的“已验证安全”范围内。

## 8. 建议修复顺序

### 第一阶段：P0 安全边界

1. 默认要求认证，或让公网/非回环明文监听必须显式确认。
2. 文件请求必须同时满足：路径安全、非忽略、树中存在、普通文件、会话有权访问。
3. 统一实现防符号链接父目录逃逸的安全文件系统操作层。
4. 为读取、写入、删除、目录创建、mtime 和本地 rename 添加恶意路径测试。

### 第二阶段：P1 同步一致性

1. 让周期性全量对账核对汇端真实磁盘。
2. 默认增量模式禁用会移除旧路径的 rename 优化。
3. 忠实镜像模式在 rename 前重新验证本地旧文件哈希。
4. 明确处理 file-to-dir 和 dir-to-file。
5. watcher 注册失败必须降级进入 tier2。

### 第三阶段：配置、性能和工程质量

1. 集中验证所有 CLI/YAML 数值和关键字段。
2. YAML 启用 `KnownFields(true)`。
3. 修复大目录分页、changed_dirs O(N^2) 和 tier2 假变化。
4. 修复 DB 删除计数和服务路径转义。
5. PR/main CI 加入测试和 race。
6. 增加双进程端到端测试与协议 fuzz 测试。

## 9. 建议 Claude 优先复核的最小用例

为了避免复核只停留在静态阅读，建议优先补以下测试：

1. 自定义客户端直接请求 `.git/config`、`.local-mirror/cache.db` 和自定义忽略文件。
2. 源端根内创建 `link -> 外部目录`，请求 `link/file`，确认是否越界读取。
3. 汇端根内创建 `link -> 外部目录`，由伪造源下发 `link/file`，确认是否越界写入。
4. 首次同步后不重启汇端，手工删除/修改/新增文件，再触发 `fullScan`。
5. `--allow-delete=false` 时在源端重命名文件，确认汇端旧路径是否被移除。
6. 源端将文件替换为同名目录，再将目录替换为同名文件。
7. 模拟 `Watcher.Add` 返回错误，确认目录是否进入 tier2。
8. 以 `-f 0` 启动源端并请求非空文件，观察发送循环。
9. YAML 中写 `secrect:`，确认当前解析是否静默成功并转为明文。
10. 删除批次同时传入父目录和子文件，检查 `file_count`/`dir_count`。

---

## 10. Claude 校对后的落地排序（覆盖 §8，供修复时直接照做）

校对结论：**16 条全部成立**。按「本机部署真实威胁 × 修复代价」重排，与 §8 的差别是把
**SEC-02 单独拎到最前**（它是唯一穿透 PSK 的越权），并把 SEC-03/04 合进同一层的
「no-follow 文件系统边界层」一次做完。

| 批次 | 条目 | 为什么这个顺序 |
|---|---|---|
| **① 立即** | **SEC-02** | 唯一「已握手对端仍可越权读」，PSK/WG 挡不住；改动集中在 `handleFileRequest` 一处，性价比最高。同时基本关闭 SEC-03 的可达性 |
| **② 一起做** | **SEC-03 + SEC-04** | 同一套 no-follow 目录句柄边界层（`openat2 RESOLVE_BENEATH` / 逐组件），读写删改一次收口；配套恶意路径测试（§9 用例 2/3） |
| **③ 一致性** | **COR-02 → COR-01 → COR-03 → COR-04** | COR-02 是「默认不删」被打破，语义正确性优先且改动小；COR-01/03/04 依次收口漂移、类型互换、watcher 丢目录 |
| **④ 加固网络形态** | **SEC-01** | 非回环默认要求密钥 + README 示例补密钥；现网已被 PSK+WG 缓解，故排在一致性之后 |
| **⑤ 配置/工程** | **CFG-01/02 → ENG-01 → PERF-03 → DB-01/PERF-02/PERF-01/SVC-01** | CFG 参数校验下沉解析层；ENG-01 上 CI 测试后，后续所有修复才有回归网兜底；PERF-03 注意 v2.2.3 未覆盖 |

**开工前两条纪律**：
1. **先落 ENG-01 的一半**——哪怕只在 PR CI 加 `go test ./...`，也让后面每条修复自带回归验证；
   §9 的 10 个最小用例正好是 SEC-02/03/04、COR-02/03、COR-04、CFG-01/02、DB-01 的红线测试，
   建议边修边补，修一条补一条。
2. **改代码同步改 §6 的四处文档措辞**，别让 README 继续承诺代码做不到的事。

---

本报告由 **Codex** 基于 `v2.2.3` 源码生成，未修改同步实现代码。
Claude 已于 2026-08-07 对全部 16 条逐一校对并就地标注（`> **✅ Claude 校对**` 区块 + §10 排序），
同样未改动任何实现代码。
