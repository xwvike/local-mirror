# 安全审计（投产前）

> 2026-07-11 审计。触发动机：项目功能已趋完整、拟投入生产使用前，系统排查
> 致命故障模式（网络风暴、日志爆盘、文件丢失、协议设计缺陷）。
> 审计对象为当时的 `feat/yaml-multi-task` 分支（含自动发现、TUI、自定义忽略、
> YAML 多任务四项新功能）。

四条发现按严重程度排列。#1 为投产硬门槛，#2/#3 建议一并修复，#4 记录在案。

**状态（2026-07-11 修复）**：#1/#2/#3 已修复并通过单测与回归验证；#4 记录在案，
文档提示不可信网络用 `-k` + `-r`。各条末尾标注了具体修复。

---

## #1 【致命】路径穿越——客户端不校验服务端下发的路径

**位置**：
- `internal/mirror.go:112` `os.RemoveAll(filepath.Join(config.StartPath, v.Path))`（删除）
- `internal/mirror.go:132-133` `os.MkdirAll(filepath.Join(config.StartPath, v.Path))`（建目录）
- `internal/mirror.go:182` `os.Chtimes(filepath.Join(config.StartPath, v.Path), ...)`（改 mtime）
- `internal/mirror.go:348-351` `os.Rename` 重命名（oldFull/newFull 均来自 diff）
- `internal/network/client.go:485` `filepath.Join(config.StartPath, filePath)`（下载落盘）

**根因**：以上所有落盘路径都是 `filepath.Join(config.StartPath, v.Path)`，而 `v.Path`
完全来自服务端下发的目录树 JSON（`UnmarshalRealityTree` → 服务端可控），**没有任何
"最终路径必须落在 StartPath 之内"的校验**。`filepath.Join` 会 `Clean` 掉 `..`，
因此服务端下发一个 `Path = "../../../Users/foo/.zshrc"` 的节点即可让最终路径逃出
同步根。

**失败场景**：
- create/modify（无需 `--allow-delete`，默认即中招）：服务端把内容**写到同步目录
  外的任意位置**——覆盖 `~/.zshrc`、`~/.ssh/authorized_keys` 等 = 任意代码执行。
- delete（`--allow-delete` 开启时）：`os.RemoveAll` **删除同步目录外的任意文件/目录**。

**可达性**：任何 mirror 连接的服务端都可触发。未设 `-k` 时握手无认证；而新加的
自动发现会让 `-r` 留空的 mirror **自动连接局域网里扫到的服务端**——局域网内一台
恶意/被攻陷/有 bug 的机器广播一个发现应答，即可让 mirror 连上并覆盖其家目录任意
文件。relay 链中一个被攻陷的上游同理污染整条下游。正常服务端由 `RelPath` 生成的
是干净相对路径、不含 `..`，故日常运行不触发——但这是"协议信任了不该信任的输入"
的典型设计漏洞。

**修复方向**：新增集中的路径安全校验（如 `safeJoin(root, rel) (string, error)`）：
`Clean` 后确认结果仍在 `root` 之内，拒绝含 `..`、绝对路径、或解析后逃逸的相对路径。
所有落盘点（下载、建目录、删除、改 mtime、重命名的新旧两端）都必须先过此校验，
命中则拒绝该 diff 项并记录告警，不中断其余同步。

**✅ 已修复**：新增 `internal/safety/paths.go` 的 `SafeJoin(root, rel)`——`Clean`
后校验最终路径等于 root 或以 `root+分隔符` 为前缀（杜绝 `/srv/sync` vs
`/srv/sync-evil` 的前缀陷阱），拒绝绝对路径与逃逸相对路径。接入点：
`mirror.go` 的删除/建目录/改 mtime/重命名两端、`client.go:DownloadFile` 入口
（越界直接拒绝，绝不向服务端发起请求也绝不落盘）。单测 `safejoin_test.go`
覆盖 `../etc/passwd`、多级逃逸、绝对路径、前缀兄弟目录；端到端回归确认正常
同步零误伤。

---

## #2 【高】日志无轮转 → 磁盘耗尽

**位置**：`internal/logger/initLogger.go:39` `os.OpenFile(LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)`

**根因**：日志文件一路 `O_APPEND` 追加，**没有任何轮转、大小上限或清理机制**。

**失败场景**：error 级别量小尚可，但 YAML 的 `defaults` 常用 `info`、调试用 `debug`，
监督进程的子任务也继承该级别，长期运行必然写满磁盘。磁盘写满后进程自身及同机
其他服务都会异常。

**修复方向**：基于大小的轮转——单文件封顶（如 10MB），超过则滚动为 `error.log.1`、
`error.log.2`…，保留最近 N 个（如 3 个）。倾向手写轮转 writer 而非引入 lumberjack
依赖（与项目少依赖风格一致），封装为实现 `io.Writer` 的类型接入现有 `io.MultiWriter`。

**✅ 已修复**：新增 `internal/logger/rotate.go` 的 `rotatingWriter`（手写，无新依赖）——
单文件封顶 10MB，超限滚动 `error.log.1..3`，保留最近 3 个，最旧丢弃；接入
`initLogger` 的 `io.MultiWriter`。重开时按已有文件大小接续计数。单测覆盖轮转触发、
保留数上限、默认值、重开接续。

---

## #3 【中】协议子长度字段无界分配 → OOM 崩溃

**位置**：
- `internal/network/protocol.go:612` `msg.Changes = make([]string, changeCount)`（changeCount uint32）
- `internal/network/protocol.go:287` `msg.Data = make([]byte, msg.DataLength)`（DataLength uint32）
- `internal/network/protocol.go:465` `msg.Data = make([]byte, msg.DataLength)`（TreeResponse）

**根因**：解码时直接拿线上 uint32 长度 `make()`，**未与实际剩余字节数（`buf.Len()`）
比对**。`receiveMessage` 的 `MaxBodyLength = 64MB` 只约束外层消息体，但内层子长度
可远大于实际 body，而 `make()` 在 `io.ReadFull` 发现字节不足之前就已执行。

**失败场景**：一个 20 字节的消息声称 `changeCount = 0xFFFFFFFF` → 尝试分配
约 64GB（`[]string` 每项 16 字节）→ 进程 OOM 崩溃。`DataLength` 类同（最多 ~4GB）。
这些解码均在客户端接收服务端消息的路径上（服务端侧解码用 uint16，有界安全），
故"恶意/有 bug/自动发现到的服务端可崩客户端"。

**修复方向**：每处 `make` 前用 `buf.Len()` 卡边界——元素至少占 K 字节时，
`count > buf.Len()/K` 直接返回解码错误（连接会被上层判为需重建）。

**✅ 已修复**：三处解码在 `make` 前加入边界校验（`protocol.go`）——`DataLength`
不得超过剩余字节；`changeCount` 不得超过剩余字节的一半（每条至少 2 字节长度
前缀）。单测覆盖谎报超大长度被拒、合法消息仍正常往返。

---

## #4 【低】UDP 发现反射放大 + 默认无认证

**位置**：`internal/network/discovery.go` 响应器（`handleProbe` / 应答循环）

**根因**：未设 `-k` 时，响应器对任何结构合法的探测都应答，且应答回探测包的源地址。
36 字节探测 → 最多 610 字节应答，约 17 倍放大；源地址可伪造 → 反射放大向量。

**评估**：局域网范围危害有限；不构成自我循环风暴（服务端之间不互相探测，探测/应答
是不同 kind，无放大回路）。结合自动连接，恶意响应器可诱使 mirror 连接（但真正的
危害由 #1 承载，本条本身只是放大）。

**处置**：优先级低，记录在案。真正加固需按源地址限速；当前接受其局域网范围内的
存在，文档提示跨网段/不可信网络用 `-k` + `-r` 显式指定。

---

## 非问题（审计确认安全，记录备查）

- **reality（服务端）不会通过协议丢源文件**：全局排查所有文件系统写/删操作
  （`os.Remove*`/`Rename`/`Create`/`WriteFile`/`Truncate`），**服务端一侧一个都没有**——
  全部位于客户端（`mirror.go`、`client.go` 下载路径）。服务端对源目录严格只读：
  `BuildFileTree` 只遍历+算哈希，watcher 只收事件，serve 只从 DB 读；唯一的写在
  `.local-mirror/`（缓存 DB + 日志），不触及用户数据。
- **TCP 无重连风暴**：`mirror.go:444` 有指数退避（5s→×1.5→封顶 60s），叠加 item/dir
  重试上限，不会忙循环重连。
- **外层消息体有上限**：`receiveMessage` 的 `MaxBodyLength = 64MB` 挡住最简单的巨包分配。
- **同目录多实例安全**：bbolt 按目录文件锁，两个实例抢同一 `.local-mirror` 会被拒绝。
- **同步根内的批量误删**：属已知设计风险，已由三级安全阶梯覆盖（见下）。#1 是其
  补充——防的是逃出同步根的删除/写入。

## 后续加固：三级安全阶梯（2026-07-11）

审计之后进一步发现：原关键路径检查（`CheckDeletableRoot`）**只在 `--allow-delete`
时才跑**，因此默认模式下 `-p /etc` 直接放行，而同步会**覆盖**已存在文件——等于
默认就能静默覆盖系统文件。收紧为三级阶梯（`safety.CheckSyncSafety`）：

- **默认**：非关键路径只同步不删除；**关键路径连同步都拒绝**（原为放行）。
- **`--allow-critical`**：解锁关键路径同步（仍不删除），并开启**覆盖前快照**
  （`SnapshotBeforeOverwrite`：首次覆盖前把原文件复制到 `.local-mirror/backups/<rel>`，
  只存最初的原始版本，快照失败则中止该文件覆盖）。
- **`--allow-delete`**：关键路径上需与 `--allow-critical` 叠加才允许删除；非关键
  路径单给即可（行为不变）。

快照用整文件复制而非硬链接：硬链接共享 inode，原地改写会连快照一起改（已被单测
抓到），复制的正确性不依赖覆盖方式。每文件仅首次覆盖复制一次。

## 第二轮审计（2026-07-13）：健壮性加固

主动复审并发/协议/资源面，未发现新的致命缺陷（首轮已覆盖路径穿越、OOM、日志）。
以下为加固项：

- **✅ 服务端连接数上限**：`Start` 的 accept 循环原本无限 `go handleConnection`，
  未设 `-k` 时无认证，可被无限开连接耗尽 goroutine/内存。加带缓冲信号量
  （`maxConcurrentConnections = 256`），非阻塞获取、满则拒绝关闭；`handleConnection`
  退出释放。
- **✅ 删除死代码 `FileClient.Ping()`**：全仓零调用，README 曾宣称"周期心跳"名不副实。
  存活性实由长轮询往返（≤50s）+ TCP keepalive（30s）+ 服务端 90s 空闲超时覆盖，
  心跳冗余。README 已更正；服务端 `handlePingRequest` 与 `MsgTypeReverify` 一并
  留作已文档化的死路径，待未来协议清理统一移除。

## 第三轮（2026-07-13）：真实网络测试发现的缺陷修复

六台真机两轮网络测试（详见 [docs/REAL_NETWORK_TEST_2026-07-13.md](docs/REAL_NETWORK_TEST_2026-07-13.md)）发现两个缺陷，
均已修复：

- **✅ 关键路径判定遗漏"子目录"方向**（高危）：`IsCriticalRoot` 原本只检测
  "同步根等于或包含关键路径"，`-p /etc/nginx` 这类落在系统目录树**内部**的
  同步根完全不设防。照字面加反向检测会退化（一切路径都是 `/` 的子目录），
  故把关键路径分为两类：**系统目录树**（`/etc`、`/usr`、`/var` 等）双向检测、
  子目录同样命中；**容器目录**（`/`、`~`、`/home`、`/Users` 等）仅自身命中，
  家目录子文件夹等日常同步目标不受限（语义经用户确认）。连带修复 `normalize`
  对尚不存在路径的符号链接前缀解析（macOS `/etc → /private/etc`）。
- **✅ 大文件写入阻塞全局变更检测**（中，响应性）：`eventFilter` 原对每个
  Write 事件在唯一的事件消费 goroutine 里同步全量重哈希，流式写入的大文件
  会拖垮整个同步根的实时性（实测 300MB 写入卡满 50 秒长轮询）。改为按路径
  防抖 1 秒后在定时器 goroutine 里做一次最终哈希落库；数据正确性本就不依赖
  该缓存哈希（服务端响应请求时现算），修复后大文件写入期间旁路小文件 3 秒
  内到达。

**已知局限（记录在案，暂不处理）**：
- **tier2 冷目录修改检测**基于 size+mtime 轮询：同尺寸 + 同一秒内的修改会漏
  （tier1 的 fsnotify 不受影响）。与 rsync 不带 `--checksum` 同性质。
- **修改检测在两侧哈希都为空时**（仅建树哈希计算失败的边角）会把同尺寸文件判为未变。
- **非 `ErrConnection` 的 handler 错误**（如 `handleTreeRequest` 的 DB 读失败）只 log
  不关连接，客户端会阻塞到读超时才重连——是"停顿→超时→重连"的不干净自愈，非数据损坏。
