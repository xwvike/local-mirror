# 发行与分发方案

> 目的：为 local-mirror 定一套三平台（macOS / Linux / Windows）统一的发行方案。
> 本文是**方案与决策记录**，不是操作手册。
>
> 背景：TODO.md 的「发行打包」条目此前只列了方向（goreleaser / Homebrew / apt）。
> 本文把它具体化，并补上 Windows 侧的结论。
>
> **2026-07-16 评审通过，已落地**：`.goreleaser.yaml` + `.github/workflows/release.yml`
> 进主仓库，tap（xwvike/homebrew-tap）与 bucket（xwvike/scoop-bucket）仓库已建。
> 评审改动两处：§2 的 `brews` 块换成 `homebrew_casks`（goreleaser v2.16 起
> `brews` 已弃用，cask 是分发预编译二进制的官方推荐，未签名二进制经
> postflight 去 quarantine）；§4 的 token 结论纠正（见该节）。
> 待确认清单的裁决记录在 §7。

## TL;DR

- **一份 goreleaser 配置驱动全部三平台**，git tag 触发 CI 自动发布，不再手工跑
  `build.sh` / `build.ps1`。
- macOS → **Homebrew tap**；Linux → **nfpm 出 deb/rpm**；
  Windows → **Scoop 自定义 bucket**（= mac 自定义 tap 的对称方案，无需签名）。
- 所有渠道的**唯一事实源都是 GitHub Releases 的压缩包 + checksums**，
  各包管理器只是指向它的一层薄封装。
- **Windows 不做代码签名**：对终端 CLI 来说未签名基本无摩擦，理由见
  [§5 签名与 SmartScreen](#5-签名与-smartscreen的真相)。

## 1. 现状

- 单文件静态二进制（`CGO_ENABLED=0`），无运行时依赖。
- 版本号：`git describe --tags --always --dirty` → 经 `-ldflags "-s -w -X main.version=…"`
  注入 `main.version`。goreleaser 沿用同一机制，**无需改代码**。
- 现有 `build.sh` / `build.ps1` 做手工交叉编译到 `dist/`，覆盖 9 个平台组合。
  落地 goreleaser 后，这两个脚本降级为「本地快速构建 / 调试」用途，
  正式发行不再依赖它们（是否保留见 §7 待确认）。

## 2. 总体方案：goreleaser 一份配置

选 goreleaser 的原因：它对本项目要用的所有渠道都有**原生生成能力**，
一次 `git tag` + CI 触发即可同时产出——

| 产物 | goreleaser 配置块 | 对应平台 |
|---|---|---|
| 跨平台压缩包 + checksums.txt | `archives` / `checksum` | 全部（事实源） |
| Homebrew cask（推到 tap 仓库） | `homebrew_casks`（`brews` 已弃用） | macOS |
| deb / rpm | `nfpms` | Linux |
| Scoop manifest（推到 bucket 仓库） | `scoops` | Windows |
| winget manifest（可选） | `winget` | Windows |

即：**mac 的 tap、win 的 bucket、linux 的包，都能从同一份 `.goreleaser.yaml`
生成并自动推送**，无需各平台单独维护脚本。

## 3. 各平台方案

### 3.1 通用底座（已定）

每个 tag 发布时，GitHub Releases 上挂：

- `local-mirror_<version>_<os>_<arch>.tar.gz`（Unix）/ `.zip`（Windows）
- `checksums.txt`（SHA256）

这是所有包管理器指向的源头，也是「不想用包管理器的人直接下载解压」的兜底。
**这一层无论如何都要有。**

### 3.2 macOS —— Homebrew tap（建议：并入 goreleaser）

- tap 此前并不存在（评审时纠正的过时假设），已新建 `xwvike/homebrew-tap`，
  由 goreleaser 的 `homebrew_casks` 块自动生成 cask 并推送（`Casks/` 目录），
  版本、URL、SHA256 全自动，人不手改。
- 用户侧：`brew tap xwvike/tap` + `brew install local-mirror`。
- 支持 Intel + Apple Silicon 双架构（cask 内按 on_intel / on_arm 分流）。
- 未签名二进制经 cask postflight 去 quarantine 标记，绕开 Gatekeeper 拦截
  （formula 时代无此问题，cask 需要显式处理——配置里已带）。

### 3.3 Linux —— nfpm（deb/rpm，已定方向）

- goreleaser `nfpms` 块直接出 `.deb` / `.rpm`，随 Release 一起挂出。
- 包内可顺带装 `deploy/local-mirror.service`（systemd unit）到
  `/lib/systemd/system/`，装完即可 `systemctl enable`。
- **待确认**：是否要搭自建 apt / dnf 源（额外维护成本），
  还是先只挂 `.deb`/`.rpm` 文件让用户 `dpkg -i` / `rpm -i`？
  建议一期先只出文件，有需求再上源。

### 3.4 Windows —— Scoop 自定义 bucket（主推，建议）

**这是 mac 自定义 tap 在 Windows 上的对称方案**，也是本文相对 TODO 的新结论：

- Scoop 专为「绿色 CLI 工具」设计：**不需签名、不需管理员权限、不碰注册表、
  不弹 UAC**——直接绕开未签名的所有顾虑。
- 自定义 bucket = 一个装 JSON manifest 的 git 仓库（如 `scoop-bucket`）。
  用户：`scoop bucket add local-mirror <repo>` → `scoop install local-mirror`，
  心智模型与 `brew tap` + `brew install` 完全一致。
- manifest 由 goreleaser `scoops` 块生成并推送；配 `checkver` + `autoupdate`
  后 `scoop update` 能自动追踪新 Release。
- **待确认**：bucket 用独立仓库（`local-mirror-scoop-bucket`）还是复用主仓库子目录？
  独立仓库更干净，推荐独立。

### 3.5 Windows —— winget（可选，二期再说）

- winget **接受未签名的 portable(zip) 包**，不是「别想了」。对单文件 CLI 用
  `InstallerType: portable` 是最顺的路径，goreleaser 有 `winget` 生成块。
- 代价：要向 `winget-pkgs` 提 PR、过微软审核 bot，且其验证流水线会跑
  Defender/SmartScreen 扫描——万一撞上 Go 二进制误报（见 §5）会被卡。
- **结论**：优先级低于 Scoop。等工具稳定、想要 winget 生态的可发现性时再做。

### 3.6 Windows —— Chocolatey（不做）

公共仓库审核慢且挑，偏 GUI 安装器场景，对本项目性价比低。
除非将来有明确需求或要自建私有 feed，否则**不做**。

## 4. 版本与 CI（建议）

- 触发：推送 `v*` tag（如 `v1.0.0`）→ GitHub Actions 跑 `goreleaser release`
  （workflow 里发布前先 `go test ./...` 拦一道）。
- 版本注入：沿用现有 `-ldflags -X main.version=…`，值取 tag（goreleaser 自动带）。
- secrets（评审时纠正）：Actions 自带的 `GITHUB_TOKEN` **只对当前仓库有权限**，
  与 owner 是否相同无关——推 tap / bucket 这两个独立仓库**必须用 PAT**。
  用 fine-grained PAT，只授权 `homebrew-tap` 与 `scoop-bucket` 两个仓库的
  Contents 读写，存为主仓库 secret `TAP_GITHUB_TOKEN`。

## 5. 签名与 SmartScreen（的真相）

这是给 mac 侧解释「Windows 为什么不签名也 OK」的部分：

- 那个吓人的「Windows 已保护你的电脑」蓝框（SmartScreen）**主要在 GUI 双击
  刚下载的 .exe 时触发**（基于 Mark-of-the-Web + 应用信誉）。
  **从已打开的终端里运行 CLI 的 .exe，一般不走那条应用信誉检查**——
  local-mirror 作为命令行工具、经 Scoop 安装、终端里运行，绝大多数情况下
  根本不弹 SmartScreen。未签名对 CLI 的实际摩擦远小于直觉。
- 真正要留意的是 **Defender 对 Go 网络类二进制偶发的启发式误报**，与签名、
  打包方式都无关。真遇到走 Microsoft 误报申诉加白即可。
- **若将来要签名**：别买传统 EV 证书（强制硬件 token，贵又烦）。看
  **Azure Trusted Signing**（约 $10/月，云端签名，可接 CI），门槛是主体
  成立满 3 年或走组织认证。
- 注意：sigstore/cosign 是**供应链验证**，与 SmartScreen 的 Authenticode
  是两个世界，别指望它消除 SmartScreen。
- **本期结论**：不签名。等有真实分发量、且够 Azure Trusted Signing 资格时再评估。

## 6. 仓库结构（建议）

```
local-mirror/                 主仓库（含 .goreleaser.yaml、CI workflow）
homebrew-<tap>/               Homebrew tap（goreleaser 自动推 formula）
local-mirror-scoop-bucket/    Scoop bucket（goreleaser 自动推 manifest）
```

tap / bucket 仓库只存自动生成的清单，人不手改。

## 7. 待确认清单（2026-07-16 评审裁决）

1. **落地 goreleaser** 统一发行，`build.sh`/`build.ps1` 降为本地调试用——**同意**。
2. **macOS tap**：此前不存在，新建 `xwvike/homebrew-tap`，goreleaser 全自动维护。
3. **Linux**：**一期只挂 deb/rpm 文件**，不自建 apt/dnf 源，有需求再上。
4. **Windows Scoop bucket**：**独立仓库** `xwvike/scoop-bucket`
   （与 homebrew-tap 命名对称，可容纳将来的其他工具）。
5. **winget**：**二期**。
6. **签名**：**本期不签名**（macOS 侧 cask postflight 去 quarantine 兜住）。
7. **CI token**：tap/bucket 与主仓库同账号，但 `GITHUB_TOKEN` 仍推不了它们
   （权限以仓库为界，非以 owner 为界）——**用 fine-grained PAT**，
   见 §4。
8. **首个 tag**：**`v0.9.0`** 先把整条流水线走通，产物验证无误后再打 `v1.0.0`。

## 8. 落地记录

- `.goreleaser.yaml`：builds（linux/darwin/windows × amd64/arm64，CGO 关，
  trimpath）+ archives + checksums + nfpms（deb/rpm，systemd unit 作为示例
  文档进 /usr/share/doc）+ homebrew_casks + scoops。本地
  `goreleaser release --snapshot --clean` 验证全部产物生成无误。
- `.github/workflows/release.yml`：v* tag 触发，test → goreleaser release。
- 许可证：评审时发现仓库没有 LICENSE，补 MIT（cask/scoop/nfpm 的 license
  字段与公开分发都依赖它）。
- 后续待办：winget（二期）、launchd plist 示例（见 TODO.md）。
