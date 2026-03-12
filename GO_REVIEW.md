# Go 语法与特性回忆清单
> 基于 local-mirror 项目代码，结合具体功能场景

---

## 1. 包与模块系统

### 1.1 package 声明与 import
```go
// 每个文件必须有 package 声明
package app

// import 分组：标准库 / 本地包 / 第三方包（goimports 自动排序）
import (
    "fmt"
    "os"

    "local-mirror/config"
    "local-mirror/internal/tree"

    log "github.com/sirupsen/logrus"  // 别名导入：解决包名冲突
)
```

**项目中的例子：** `main.go` 把 `internal` 包别名为 `app`，把 `logrus` 别名为 `log`

### 1.2 internal 包
```
internal/       ← 只有 local-mirror 模块内部可以 import
pkg/            ← 可以被任何模块 import（公共工具）
```
Go 编译器强制执行：外部模块尝试 import `internal/` 下的包会报编译错误。

### 1.3 init() 函数
```go
// config/config.go
func init() {
    // 程序启动时自动执行，早于 main()
    // 同一包内多个 init() 按文件名字母序执行
    Mode = flag.String("mode", "reality", "运行模式")
}

// main.go
func init() {
    config.InstanceID = utils.GenerateRandomNum()  // 生成实例ID
    config.StartTime = time.Now().Unix()
}
```
**执行顺序：** 依赖包的 `init()` → 本包的 `init()` → `main()`

---

## 2. 基础类型

### 2.1 数值类型与字面量
```go
// protocol.go：协议常量用十六进制字面量
const (
    MagicNumber uint32 = 0xFBE322A8
    MsgTypeHandshake uint16 = 0x0001
)

// config.go：位掩码表示模式
const (
    RealityMode = 0x0001
    MirrorMode  = 0x0002
)
```

### 2.2 自定义类型 + iota（本项目修复后的写法）
```go
// client.go
type ConnectionState uint8  // 基于 uint8 的新类型，类型安全

const (
    Waiting    ConnectionState = iota  // 0
    Online                             // 1
    Offline                            // 2
    Deprecated                         // 3
)

// 好处：编译器会阻止 fileClient.State = 99 这样的非法赋值
// 而 var (Online uint8 = 0x01) 的写法允许任意 uint8 赋值
```

**iota 规则：**
- 每个 `const` 块独立从 0 开始
- 每行递增 1
- 可以参与表达式：`1 << iota` 生成 1, 2, 4, 8...

---

## 3. 复合类型

### 3.1 结构体（Struct）
```go
// treeDB.go：Node 是整个文件树的核心数据结构
type Node struct {
    ID       string    `json:"id"`         // 反引号内是结构体标签
    Path     string    `json:"path"`
    IsDir    bool      `json:"is_dir"`
    Size     uint64    `json:"size"`
    ModTime  time.Time `json:"mod_time"`   // 嵌入标准库类型
    Depth    int       `json:"depth"`
}

// 初始化：字段名:值（未写的字段为零值）
rootNode := &Node{
    ID:    uuid,
    Path:  ".",
    IsDir: true,
}
```

**结构体标签用途：** `json:"..."` 控制序列化的键名，`encoding/json` 读取这些标签。

### 3.2 Slice（动态数组）
```go
// buildFileTree.go：收集所有节点
var allNodes []*Node          // nil slice，声明但未分配
allNodes = append(allNodes, node)  // append 自动扩容

// 预分配容量（性能优化）
result := make([]string, 0, len(input))

// 切片操作
batch := allNodes[i:end]      // 左闭右开区间，共享底层数组

// slices 包（Go 1.21+）
createEventCache = slices.Delete(createEventCache, 0, len(createEventCache))
```

**关键概念：** slice 是对底层数组的引用（指针+长度+容量），`append` 超过容量时会重新分配。

### 3.3 Map
```go
// diffQueue.go：O(1) 查找节点
bMap := make(map[string]tree.Node)
for _, node := range b {
    bMap[node.Path] = node  // 写入
}

// 读取 + 存在性检查（两值赋值模式）
nodeB, exists := bMap[pathA]
if !exists { ... }

// 零内存占用的 Set 惯用法
seen := make(map[string]struct{})
seen[v] = struct{}{}   // struct{} 不占内存
if _, ok := seen[v]; !ok { ... }
```

### 3.4 数组（固定长度）
```go
// protocol.go：协议中的固定长度字段
type FileResponseMessage struct {
    SessionID [16]byte  // 固定16字节，值类型（复制时整体复制）
    FileHash  [32]byte  // Blake3 哈希值
}

// 访问底层字节：用切片表达式
buf.Write(msg.SessionID[:])  // [16]byte → []byte
```

---

## 4. 函数

### 4.1 多返回值
```go
// 惯用模式：(结果, error)
func CalcBlake3(path string) ([32]byte, error) {
    var result [32]byte
    f, err := os.Open(path)
    if err != nil {
        return result, err  // 返回零值 + error
    }
    defer f.Close()
    // ...
    return result, nil  // 成功时 error 为 nil
}
```

### 4.2 方法（接收者函数）
```go
// 指针接收者：修改接收者状态，或接收者很大时避免复制
func (s *Stack[T]) Push(value T) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.data = append(s.data, value)
}

// 值接收者：不需要修改状态
func (f *SimpleFormatter) Format(entry *log.Entry) ([]byte, error) {
    // 实现 logrus.Formatter 接口
}

// 规则：同一类型的方法集要统一用指针接收者或值接收者
```

### 4.3 匿名函数与闭包
```go
// app.go：信号处理关闭时的清理函数
defer func() {
    if *config.Mode == "reality" {
        _watcher.Close()
    }
}()  // 立即调用的匿名函数（IIFE）

// mirror.go：goroutine 中的闭包捕获外部变量
go func() {
    fullScanTicker := time.NewTicker(fullScanInterval)
    defer fullScanTicker.Stop()
    for range fullScanTicker.C {
        fullScanChan <- struct{}{}  // 捕获外部 chan
    }
}()
```

### 4.4 函数作为参数（高阶函数）
```go
// mirror.go：策略模式，把"要执行什么任务"作为参数传入
func executeTaskWithClient(
    taskName string,
    fileClient *network.FileClient,
    taskFunc func(*network.FileClient) error,  // 函数类型
) error {
    // ...
    return taskFunc(fileClient)
}

// 调用时传入 lambda
executeTaskWithClient("全量扫描", fileClient, func(client *network.FileClient) error {
    return fullScan(client)
})
```

---

## 5. 接口

### 5.1 隐式实现
```go
// logger/initLogger.go：实现 logrus.Formatter 接口
type SimpleFormatter struct{}

// 只要实现了接口要求的所有方法，就自动满足接口（无需 implements 关键字）
func (f *SimpleFormatter) Format(entry *log.Entry) ([]byte, error) {
    timestamp := entry.Time.Format("2006-01-02 15:04:05.000")
    return []byte(fmt.Sprintf("%s [%s] %s\n", timestamp, entry.Level, entry.Message)), nil
}

// 使用：
log.SetFormatter(&SimpleFormatter{})  // 传指针，因为方法是指针接收者
```

### 5.2 空接口与 any
```go
// stack.go：泛型约束中的 any 等价于 interface{}
type Stack[T any] struct { ... }

// binary.Write 接受 any（interface{}）参数
binary.Write(buf, binary.BigEndian, msg.Version)  // uint16 满足 any
```

---

## 6. 并发编程（项目核心）

### 6.1 goroutine
```go
// app.go：启动服务的后台任务
switch *config.Mode {
case "reality":
    go Reality()   // 非阻塞启动
case "mirror":
    go Mirror()
}

// 主 goroutine 继续执行，等待信号
sigChan := make(chan os.Signal, 1)
signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
<-sigChan  // 阻塞直到收到信号
```

### 6.2 Channel
```go
// 无缓冲 channel：发送方阻塞直到接收方就绪（同步）
sigChan := make(chan os.Signal, 1)  // 缓冲为1：不会丢失信号

// 有缓冲 channel：buildFileTree.go 工作池通信
nodeChan := make(chan *Node, 1000)  // 缓冲1000个节点

// 发送
nodeChan <- rootNode

// 接收（range：直到 channel 关闭）
for node := range nodeChan {
    allNodes = append(allNodes, node)
}

// 关闭（只有发送方关闭）
close(nodeChan)
```

### 6.3 select（多路复用）
```go
// mirror.go：同时等待全量扫描和变更追踪两个触发器
for {
    select {
    case <-fullScanChan:    // 哪个先就绪执行哪个
        executeTaskWithClient("全量扫描", ...)
    case <-changeChan:
        executeTaskWithClient("变更追踪", ...)
    }
    // 没有 default：阻塞等待
}
```

### 6.4 sync.Mutex / sync.RWMutex
```go
// buildFileTree.go：保护共享 slice
var mu sync.Mutex
mu.Lock()
defer mu.Unlock()         // defer 保证即使 panic 也能释放锁
allNodes = append(allNodes, node)

// client.go：读写分离（多读少写场景性能更好）
var mutex sync.RWMutex

func (cm *ConnectionManager) GetConnection() (net.Conn, error) {
    cm.mutex.RLock()       // 读锁：允许多个 goroutine 同时读
    defer cm.mutex.RUnlock()
    // ...
}

func (cm *ConnectionManager) Reconnect() error {
    cm.mutex.Lock()        // 写锁：独占
    defer cm.mutex.Unlock()
    // ...
}
```

**惯用模式：** `Lock()` 之后立即 `defer Unlock()`，放在函数开头，不要在条件分支里混用。

### 6.5 sync.WaitGroup
```go
// buildFileTree.go：等待所有 worker goroutine 完成
var wg sync.WaitGroup
for range workerCount {
    wg.Add(1)              // 启动前 Add
    go func() {
        defer wg.Done()   // 完成时 Done（用 defer 防遗漏）
        for node := range nodeChan { ... }
    }()
}
wg.Wait()                 // 阻塞直到所有 Done() 调用
```

### 6.6 工作池模式（Worker Pool）
```go
// buildFileTree.go：CPU密集型任务，池大小=CPU核数
workerCount := runtime.NumCPU()
nodeChan := make(chan *Node, 1000)

for range workerCount {       // Go 1.22+ 纯计数 range
    wg.Add(1)
    go func() {               // 每个 worker 从 channel 取任务
        defer wg.Done()
        for node := range nodeChan {
            mu.Lock()
            allNodes = append(allNodes, node)
            mu.Unlock()
        }
    }()
}
// 主 goroutine 生产数据
filepath.WalkDir(path, func(...) { nodeChan <- node })
close(nodeChan)  // 关闭触发所有 worker 退出 range 循环
wg.Wait()
```

---

## 7. 错误处理

### 7.1 error 接口与惯用模式
```go
// 标准做法：函数最后一个返回值是 error
func BuildFileTree(path string) error {
    if err := os.MkdirAll(...); err != nil {
        return err  // 向上传递
    }
    return nil     // 成功
}

// 调用方必须检查 error（Go 的设计哲学）
if err := tree.BuildFileTree(path); err != nil {
    log.Error(err)
}
```

### 7.2 错误包装与 %w
```go
// fmt.Errorf + %w：包装错误，保留原始错误链
return fmt.Errorf("failed to open database: %w", err)

// errors.Is：检查错误链中是否包含特定错误（穿透包装层）
if errors.Is(err, appError.ErrConnection) {
    fileClient.ConnectionClose()
}

// 关键区别：
// %w → errors.Is/As 可以匹配（错误链）
// %v → 仅格式化，errors.Is 无法匹配
```

**本项目修复的错误：**
```go
// 错误：两个 %w 语义混乱
return nil, fmt.Errorf("%w, failed to get connection: %w", appError.ErrConnection, err)

// 正确：%w 用于哨兵错误，%v 用于附加信息
return nil, fmt.Errorf("%w: failed to get connection: %v", appError.ErrConnection, err)
```

### 7.3 哨兵错误（Sentinel Error）
```go
// appError/errorTypes.go：预定义的命名错误
var ErrConnection = errors.New("connection error")

// 用 errors.Is 比较，不要用 == 比较包装后的错误
if errors.Is(err, appError.ErrConnection) { ... }  // 正确
if err == appError.ErrConnection { ... }            // 错误（包装后不相等）
```

### 7.4 log.Fatalf 是终止函数
```go
// Fatalf 内部调用 os.Exit(1)，之后代码永远不执行
log.Fatalf("获取当前执行文件路径失败: %v", err)
// os.Exit(1)  ← 死代码，不要写
```

---

## 8. defer

```go
// 1. 资源清理（最常见用法）
file, err := os.Open(path)
defer file.Close()   // 函数返回时执行，无论正常还是 panic

// 2. 解锁
mu.Lock()
defer mu.Unlock()    // 立即在 Lock 后写，防止忘记

// 3. 多个 defer：LIFO 顺序（后进先出）
defer fmt.Println("第三个执行")
defer fmt.Println("第二个执行")
defer fmt.Println("第一个执行")
// 输出顺序：第一个 → 第二个 → 第三个

// 4. defer 在循环中注意：每次迭代注册，函数返回才执行
// 在循环中开文件要手动 Close，不能用 defer（否则文件一直不关闭）
```

---

## 9. 泛型（Go 1.18+）

```go
// stack/stack.go：线程安全的泛型栈
type Stack[T any] struct {   // T 是类型参数，any 是约束（任意类型）
    data []T
    mu   sync.Mutex
}

func NewStack[T any]() *Stack[T] {
    return &Stack[T]{data: make([]T, 0)}
}

func (s *Stack[T]) Push(value T) { ... }
func (s *Stack[T]) Pop() (T, bool) {
    // 返回零值的惯用法
    var zero T
    return zero, false
}

// 使用时实例化（编译器推断）
diffQueue = stack.NewStack[DiffResult]()
NextLevel  = stack.NewStack[DiffResult]()
```

**泛型适用场景：** 容器（Stack、Queue、Set）、算法（Sort、Map、Filter）等与类型无关的逻辑。

---

## 10. 指针

```go
// 取地址
rootNode := &Node{...}     // 等价于 new(Node) 并赋值

// 解引用
*config.Mode               // 读取指针指向的值（flag 返回 *string）
*config.CoolDown           // *int64

// 什么时候用指针？
// 1. 需要修改调用者的数据
// 2. 大结构体避免复制开销
// 3. nil 表示"不存在"的语义
// 4. 实现接口时需要指针接收者

// nil 检查
if fileClient.connectionManage != nil {
    fileClient.connectionManage.Close()
}
```

---

## 11. 标准库重点回忆

### encoding/binary（二进制协议，protocol.go）
```go
// 写入（BigEndian = 网络字节序）
binary.Write(buf, binary.BigEndian, msg.Version)  // 自动处理字节宽度

// 读取
binary.Read(buf, binary.BigEndian, &msg.Version)  // 传指针，读入目标

// 直接操作字节切片
binary.BigEndian.PutUint64(data, value)           // 写
value = binary.BigEndian.Uint64(data)             // 读
```

### encoding/json（树结构序列化，treeDB.go）
```go
// 序列化（结构体 → JSON bytes）
nodeData, err := json.Marshal(node)    // 注意：传值而非指针也可以

// 反序列化（JSON bytes → 结构体）
var node Node
json.Unmarshal(data, &node)            // 传指针，修改目标

// 结构体标签控制字段名
type Node struct {
    IsDir bool `json:"is_dir"`   // JSON 中是 "is_dir"
    Hash  string `json:"hash"`
}
```

### path/filepath（跨平台路径，buildFileTree.go）
```go
filepath.Join(config.StartPath, v.Path)   // 路径拼接（自动处理分隔符）
filepath.Dir("/a/b/c.txt")                // → "/a/b"
filepath.Base("/a/b/c.txt")               // → "c.txt"
filepath.WalkDir(root, func(...) error)   // 递归遍历目录树
filepath.SkipDir                          // 返回此值跳过当前目录
```

### time（定时器，mirror.go）
```go
// Ticker：周期触发
ticker := time.NewTicker(10 * time.Second)
defer ticker.Stop()            // 必须 Stop，否则 goroutine 泄漏
for range ticker.C { ... }    // C 是 channel，每隔指定时间发送一次

// AfterFunc：延时执行一次（非阻塞）
timer := time.AfterFunc(5*time.Second, func() {
    // 在新 goroutine 中执行
})
timer.Stop()                   // 取消未执行的 timer

// 时间运算
elapsed := time.Since(startTime)      // Duration
duration := time.Duration(n) * time.Second
```

### net（TCP 通信，network/）
```go
// 服务端
listener, _ := net.Listen("tcp", "0.0.0.0:52345")
conn, _ := listener.Accept()           // 阻塞等待连接

// 客户端
conn, err := net.Dial("tcp", addr)     // 建立连接

// io.ReadFull：确保读满指定字节数（处理 TCP 粘包）
io.ReadFull(conn, headerBytes)         // 不会因为数据未到达就返回
```

### flag（命令行参数，config.go）
```go
// 定义参数（返回指针）
Mode = flag.String("mode", "reality", "运行模式")

// 短参数别名
flag.StringVar(Mode, "m", "reality", "同 --mode")

// 解析（在 main 开头调用）
flag.Parse()

// 使用（解引用）
*config.Mode == "reality"
```

### os/signal（优雅退出，app.go）
```go
sigChan := make(chan os.Signal, 1)
signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
<-sigChan  // 阻塞，收到信号后继续
// 执行清理逻辑...
```

---

## 12. Go 命名规范（速记）

| 场景 | 规范 | 例子 |
|------|------|------|
| 普通变量/函数 | 驼峰 | `dirCount`, `fileClient` |
| 导出（public） | 首字母大写 | `AddNodes`, `FileClient` |
| 未导出（private）| 首字母小写 | `connectionManage`, `dirCount` |
| 接口名 | 通常加 -er | `Formatter`, `Reader`, `Writer` |
| 错误变量 | Err 前缀 | `ErrConnection`, `ErrNotExist` |
| **禁止** | 下划线命名 | ~~`dir_count`~~ → `dirCount` |
| **禁止** | 全大写缩写 | ~~`URL`~~ → `Url` 或 `url`（缩写除外：`ID`, `HTTP`） |

---

## 13. 常见惯用法速记

```go
// 1. 零值 Set
seen := make(map[string]struct{})
seen[key] = struct{}{}

// 2. 函数内 defer unlock（固定模板）
mu.Lock()
defer mu.Unlock()

// 3. 类型断言（判断接口具体类型）
if state, ok := v.(ConnectionState); ok { ... }

// 4. 空白标识符（显式忽略）
uuid, _ := utils.RandomString(16)   // 不关心 error
_ = binary.Write(buf, ...)          // 有意忽略（需注释说明原因）

// 5. 变量遮蔽（:= 在内层作用域）
err := doA()
if err := doB(); err != nil {  // 新的 err，外层 err 未被修改
    return err
}

// 6. 短路求值
if cm.conn != nil && cm.isConnValid() { ... }  // conn 为 nil 时不调用 isConnValid

// 7. 多重赋值
nodeB, exists := bMap[pathA]

// 8. 闭包陷阱（循环变量捕获）
for i, v := range items {
    go func(i int, v Item) {  // 传参而非直接使用循环变量
        use(i, v)
    }(i, v)
}
```

---

## 14. 本项目修复汇总（对照学习）

| 文件 | 问题 | 原因 | 修复方式 |
|------|------|------|----------|
| `client.go` | `var Online uint8` | var 可被修改 | `const` + 自定义类型 `ConnectionState` |
| `client.go` | `GetConnection` defer 混乱 | 分支内 defer 难读 | 函数入口统一 `defer RUnlock()` |
| `client.go` | `conn, _ := GetConnection()` | nil conn 导致 panic | 检查 error 后再使用 |
| `client.go` | `fmt.Errorf("%w...%w")` | 双 %w 语义混乱 | `%w` 包装哨兵，`%v` 附加信息 |
| `client.go` | `file.Sync()` 返回值忽略 | I/O 错误被吞 | 检查并 log.Warn |
| `treeDB.go` | InitDB 第一个 err 被覆盖 | 静默失败 | if/else 分别检查两个 Update |
| `treeDB.go` | `var dir_count uint64` | 下划线命名 | 改为 `dirCount` |
| `treeDB.go` | `switch node.IsDir { case true` | bool 不适合 switch | 改为 `if node.IsDir` |
| `treeDB.go` | `if pathID != nil { exists = true }` | 冗余 if/else | `exists = pathID != nil` |
| `diffQueue.go` | `ParentID: nodeB.ParentID` | nodeB 是零值 | 改为 `nodeA.ParentID` |
| `initConn.go` | `for i := range make([]struct{}, n)` | 无意义内存分配 | `for i := 0; i < n; i++` |
| `mirror.go` | 循环清空 Stack | 忽略现有方法 | `NextLevel.Clear()` |
| `mirror.go` | `os.MkdirAll(v.Path, ...)` | 相对路径创建位置错误 | `filepath.Join(config.StartPath, v.Path)` |
| `main.go` | `Fatalf` 后接 `os.Exit(1)` | 死代码 | 删除多余的 `os.Exit` |
| `protocol.go` | `binary.Write(...)` 返回值丢弃 | 隐式忽略 | 改为 `_ = binary.Write(...)` |
