# 项目重构说明

## 重构前的问题

1. **包结构不合理**：
   - `app` 包包含了太多不同职责的代码
   - 客户端和服务端代码混在一起
   - 协议定义散落在业务代码中

2. **命名不规范**：
   - `createLink.go` 文件名不符合 Go 命名规范
   - 函数名如 `BaseOSInfo()` 应该是 `GetOSInfo()`
   - 配置文件中有拼写错误（`.gitingore` → `.gitignore`）

3. **代码组织混乱**：
   - 网络协议、文件树、客户端、服务端代码耦合严重
   - 工具函数和业务逻辑混合
   - 缺乏清晰的分层架构

## 重构后的项目结构

```
local-mirror/
├── cmd/                    # 应用入口
│   └── local-mirror/
│       └── main.go
├── config/                 # 配置管理
│   └── config.go
├── internal/               # 内部包（仅限项目内使用）
│   ├── client/            # 客户端实现
│   │   └── client.go
│   ├── server/            # 服务端实现
│   │   └── server.go
│   ├── protocol/          # 网络协议定义
│   │   └── protocol.go
│   ├── tree/              # 文件树管理
│   │   └── tree.go
│   ├── watcher/           # 文件监视器
│   │   └── watcher.go
│   └── diff/              # 差异比较
│       └── compare.go
├── pkg/                   # 公共包（可被外部使用）
│   ├── logger/            # 日志工具
│   │   └── logger.go
│   ├── utils/             # 工具函数
│   │   └── helper.go
│   └── data/              # 数据结构
│       └── stack.go
├── tools/                 # 工具脚本
├── build.sh               # 构建脚本
├── build.ps1              # Windows 构建脚本
├── go.mod
└── README.md
```

## 主要改进

### 1. 清晰的分层架构

- **cmd/**: 应用程序入口点
- **internal/**: 项目内部包，按功能模块组织
- **pkg/**: 可复用的公共包
- **config/**: 配置管理

### 2. 模块化设计

- **protocol**: 网络协议定义和编解码
- **client**: 客户端功能实现
- **server**: 服务端功能实现
- **tree**: 文件树管理和数据库操作
- **diff**: 文件差异比较算法
- **watcher**: 文件系统监视器

### 3. 改进的命名规范

- 函数名遵循 Go 命名约定
- 文件名使用下划线分词
- 导出函数使用大写字母开头
- 修复了拼写错误

### 4. 更好的代码组织

- 按职责分离不同模块
- 减少模块间的耦合
- 统一的错误处理方式
- 清晰的接口定义

## 使用方式

### 构建项目

```bash
go build ./cmd/local-mirror
```

### 运行 Reality 模式（服务端）

```bash
./local-mirror -mode=reality -loglevel=info
```

### 运行 Mirror 模式（客户端）

```bash
./local-mirror -mode=mirror -realityip=192.168.1.100 -loglevel=info
```

## 配置选项

- `-mode`: 运行模式，"reality" 或 "mirror"
- `-realityip`: 服务端 IP 地址（客户端模式必需）
- `-loglevel`: 日志级别，"debug", "info", "warn", "error"
- `-cooldown`: 冷却时间（秒）
- `-filebuffersize`: 文件缓冲区大小
- `-memfilethreshold`: 内存文件阈值

这次重构大大提升了代码的可维护性、可读性和可扩展性，使项目结构更加清晰和专业。
