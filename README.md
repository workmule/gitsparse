# gitsparse

从 Git 仓库中快速拉取**指定目录**的轻量级命令行工具。基于 Git 的 `shallow clone`（`--depth=1`）+ 本地缓存 + 目录拷贝实现，仅下载你需要的目录，大幅节省时间和带宽。

## 特性

- **浅克隆** — 使用 `--depth=1`，不拉取完整历史
- **本地缓存复用** — 同一 `repo + ref` 的 git 仓库缓存复用，二次拉取只需 `fetch + reset`，秒级完成
- **自动重试** — 网络操作（clone / fetch / LFS pull）失败或超时后自动重试
- **超时控制** — 可为每个网络操作设置超时
- **Git LFS 支持** — 自动检测 LFS 文件并按需拉取大文件
- **缓存自动清理** — 超过 TTL 的缓存条目自动清理

## 适用场景

- CI/CD 流水线中只拉取构建所需的子目录
- 从大型 monorepo 中提取部分代码
- 快速查看大型仓库中某个目录的内容
- 网络带宽受限环境下按需拉取

## 安装

### 方式一：`go install`（推荐）

```bash
go install github.com/workmule/gitsparse@latest
```

安装后，二进制文件 `gitsparse` 会放在 `$GOPATH/bin`（或 `$GOBIN`）目录下，确保该目录已加入 `PATH`：

```bash
# 确认安装
gitsparse -h
```

### 方式二：从源码构建

```bash
git clone git@github.com:workmule/gitsparse.git
cd gitsparse
go build -o gitsparse .
```

### 方式三：`go run` 直接运行

无需安装，直接运行：

```bash
go run github.com/workmule/gitsparse@latest \
    -repo https://github.com/numpy/numpy.git \
    -ref main \
    -dirs numpy
```

### 前置依赖

- **Go** 1.22+（仅构建时需要）
- **Git** 2.0+（基础 clone / fetch / checkout 即可，无版本特殊要求）
- **Git LFS**（可选，仓库包含 LFS 文件时需要）

## 使用方法

### 基本用法

```bash
gitsparse -repo <仓库URL> -ref <分支/标签/commit> -dirs <目录1,目录2,...>
```

### 参数说明

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-repo` | （必填） | Git 仓库 URL |
| `-ref` | （必填） | Git 引用：分支名、标签名或 commit SHA |
| `-dirs` | （必填） | 要拉取的目录路径，多个用逗号分隔 |
| `-output` | `.` | 输出目录 |
| `-timeout` | `1m` | 每个网络操作的超时时间；`0` = 不限时 |
| `-retries` | `3` | 网络操作失败后的重试次数 |
| `-cache-dir` | `/tmp/gitsparse-cache` | 缓存目录 |
| `-cache-ttl` | `24h` | 缓存 TTL；超过此时间的条目自动清理；`0` = 不清理 |
| `-no-cache` | `false` | 跳过缓存，强制重新克隆 |
| `-no-lfs` | `false` | 跳过 Git LFS 拉取（LFS 文件将保持为指针，非真实内容） |

### 示例

拉取 NumPy 的 `numpy` 目录：

```bash
gitsparse \
    -repo https://github.com/numpy/numpy.git \
    -ref main \
    -dirs numpy \
    -output ./output
```

拉取多个目录：

```bash
gitsparse \
    -repo https://github.com/microsoft/vscode.git \
    -ref main \
    -dirs src/vs,build \
    -output ./vscode-src
```

拉取特定 commit：

```bash
gitsparse \
    -repo https://github.com/pytorch/pytorch.git \
    -ref a1b2c3d4e5f6 \
    -dirs torch \
    -output ./torch
```

跳过缓存、设置超时和重试：

```bash
gitsparse \
    -repo https://github.com/numpy/numpy.git \
    -ref main \
    -dirs numpy \
    -no-cache \
    -timeout 10m \
    -retries 5
```

### 使用 Makefile

项目附带 Makefile，方便快速测试：

```bash
# 使用默认配置测试（拉取 numpy）
make run

# 自定义参数
make run REPO=https://github.com/pytorch/pytorch.git REF=main DIRS=torch

# 构建二进制
make build

# 清理输出
make clean

# 清理所有缓存
make clean-cache
```

## 工作流程

```
1. 浅克隆 (--depth=1 --branch <ref>)     → 仅下载最新提交，检出全部文件到缓存目录
   ├─ 首次: git clone --depth=1 --branch <ref> <repo> <cachedir>
   └─ 缓存命中: git fetch --depth=1 origin <ref> + git reset --hard origin/<ref>
2. LFS pull (如果检测到 LFS 文件)          → 按需拉取大文件（仅指定目录）
3. 拷贝到输出目录                          → 将指定子目录复制到 -output 指定位置
4. 清理过期缓存                            → 删除超过 TTL 的缓存条目
```

> **设计说明 (v2.0)**：旧版 (v1.x) 使用 `sparse-checkout` 机制（`--no-checkout` +
> `core.sparseCheckout` + 手写 `.git/info/sparse-checkout` + `read-tree`），在不同
> Git 版本 / cone 模式 / 缓存复用场景下反复出问题（文件丢失、skip-worktree 残留、
> no-op checkout 等）。v2.0 改用最简单直接的 git 用法：完整 clone 到缓存目录，
> 再拷贝指定子目录。代价是缓存目录会检出全部文件（而非只检出指定目录），但
> `--depth=1` 浅克隆本身只下载最新提交的对象，工作区文件是本地 checkout，不增加
> 网络流量。

## Git 版本兼容性

本工具 v2.0 只使用最基础的 git 命令（`clone` / `fetch` / `checkout` / `reset`），
不依赖 `sparse-checkout`、`cone 模式` 等 Git 2.25+ 特性，兼容性最好。

- **Git 2.0+**：完全支持
- 启动日志会打印 Git 版本：`[git] git version x.y.z`，便于流水线环境排查

## 缓存机制

- 缓存 key 基于 `repo URL + ref` 的 SHA-256 哈希（前 12 位）
- 缓存目录是一个完整的 git 仓库（含工作区，全量检出）
- 同一 `repo + ref` 的第二次拉取会复用缓存：`git fetch --depth=1 origin <ref>` +
  `git reset --hard origin/<ref>` 更新到最新版本，跳过 clone 步骤
- fetch 失败不致命（可能是离线运行），继续使用缓存旧版本
- 如果 `reset --hard` 失败（缓存损坏），会自动清除缓存并提示重新运行
- 可通过 `-cache-ttl` 设置缓存过期时间，或 `-no-cache` 完全禁用缓存

## License

MIT
