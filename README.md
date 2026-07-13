# gitsparse

从 Git 仓库中快速拉取**指定目录**的轻量级命令行工具。基于 Git 的 `sparse-checkout`（cone 模式）和 `shallow clone`（`--depth=1`）实现，仅下载你需要的目录，大幅节省时间和带宽。

## 特性

- **稀疏检出** — 只拉取仓库中指定的目录，无需 clone 整个仓库
- **浅克隆** — 使用 `--depth=1`，不拉取完整历史
- **本地缓存** — 同一 `repo + ref` 的克隆结果会缓存复用，二次拉取秒级完成
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
- **Git** 2.25+（支持 `sparse-checkout`）
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
| `-timeout` | `5m` | 每个网络操作的超时时间；`0` = 不限时 |
| `-retries` | `3` | 网络操作失败后的重试次数 |
| `-cache-dir` | `/tmp/gitsparse-cache` | 缓存目录 |
| `-cache-ttl` | `24h` | 缓存 TTL；超过此时间的条目自动清理；`0` = 不清理 |
| `-no-cache` | `false` | 跳过缓存，强制重新克隆 |

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
1. 浅克隆 (--depth=1 --sparse)         → 仅下载最新提交，不检出文件
2. 配置 sparse-checkout (cone 模式)     → 指定只需要的目录
3. checkout                              → 检出指定目录内的文件
4. LFS pull (如果检测到 LFS 文件)       → 按需拉取大文件
5. 拷贝到输出目录                        → 将文件复制到 -output 指定位置
6. 清理过期缓存                          → 删除超过 TTL 的缓存条目
```

## 缓存机制

- 缓存 key 基于 `repo URL + ref` 的 SHA-256 哈希（前 12 位）
- 同一 `repo + ref` 的第二次拉取会复用缓存，跳过 clone 步骤
- 如果缓存的分支已更新导致 checkout 失败，会自动清除缓存并提示重新运行
- 可通过 `-cache-ttl` 设置缓存过期时间，或 `-no-cache` 完全禁用缓存

## License

MIT
