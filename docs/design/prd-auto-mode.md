# PRD: -mode auto 自动模式选择

> 日期: 2026-09-08
> 状态: 已实现 (2026-09-08, TDD)
> 关联: docs/design/snip-mode-design.md (snip 模式设计)

---

## 问题陈述

gitsparse 目前有两种拉取模式：`full`（全量浅克隆，兼容性最好）和 `snip`（sparse-checkout cone 局部检出，要求 Git 2.25+）。

gitsparse 面向 CI/CD 流水线环境，执行机器的 Git 版本不可控（可能很旧或被发行版裁剪）。
流水线维护者无法预知目标机器的 Git 版本，只能保守地固定使用 `full` 模式，
即使机器上有新版 Git 也享受不到 snip 模式的速度与磁盘优势；
或者显式指定 `-mode snip` 后在旧 Git 机器上直接失败。

用户需要一个"由工具自己判断"的模式：启动时识别本地 Git 版本，能用 snip 就用 snip，否则自动退回 full。

## 解决方案

新增虚拟模式 `auto`（CLI 层值，非注册的拉取模式）：

- 用户传 `-mode auto` 时，gitsparse 在启动阶段解析 `git --version` 输出：
  - Git ≥ 2.25（支持 sparse-checkout --cone）→ 选用 `snip`
  - Git < 2.25 或版本串解析失败 → 保守退回 `full`，并打印告警与决策原因
- 决策结果写入日志（选中哪个模式、依据的版本号），随后流程与显式指定该模式完全一致
- 缓存按解析后的实际模式名（snip/full）隔离与复用，`auto` 本身不产生独立缓存分区

## 用户故事

1. 作为 CI 流水线维护者，我想用 `-mode auto` 让 gitsparse 自动选择拉取模式，以便同一份流水线配置无需修改即可在不同 Git 版本的机器上运行。
2. 作为 CI 流水线维护者，当机器 Git ≥ 2.25 时 auto 应选择 snip，以便大型 monorepo 场景获得更快的检出速度和更低的磁盘占用。
3. 作为 CI 流水线维护者，当机器 Git < 2.25 时 auto 应自动退回 full，以便任务不因模式不兼容而失败，也不需要我手动探测版本后改参数。
4. 作为 CI 流水线维护者，我想在日志中看到 auto 的决策结果与依据（当前 Git 版本、选中模式、降级原因），以便排查"为什么这台机器跑的是 full"。
5. 作为 CI 流水线维护者，当 `git --version` 输出格式怪异、版本解析失败时，auto 应保守选择 full 并打印告警继续执行，以便任务不因解析问题中断。
6. 作为用户，我想在 `-mode` 的 flag 帮助文本和 `-list-modes` 输出中看到 auto 及其语义说明，以便发现并理解这个能力。
7. 作为用户，auto 解析出的模式应与显式指定该模式共享同一份缓存（auto 不引入独立缓存 key），以便同一仓库在 auto / 显式模式间切换时不重复下载。
8. 作为用户，显式 `-mode snip` 在低版本 Git 上的行为不因 auto 的引入而改变（仍在 FetchRepo 入口报清晰错误引导改用 full），以便错误信息保持明确。
9. 作为 gitsparse 开发者，auto 不应注册为新的 Puller 实现，以便不破坏"新增拉取模式 = 新包 + init 注册"的架构语义，也避免缓存 key 被固化为 "auto"。
10. 作为 gitsparse 开发者，auto 的决策逻辑应是一个输入版本串、输出模式名的纯函数，以便用表驱动测试覆盖各种版本格式而无需真实执行 git。
11. 作为用户，`-mode` 默认值保持 `full` 不变，以便现有流水线行为零变化，auto 作为 opt-in 逐步采用。

## 实现决策

- **auto 是 CLI 层虚拟值，不注册 Puller**。理由：
  - 注册表中的模式名会参与缓存 key（`Run` 内以 `p.Name()` 覆盖 `opts.Mode`），
    若注册 auto 会产生独立 "auto" 缓存分区，与 full/snip 互不复用，造成无谓缓存失效。
  - auto 无自己的拉取逻辑，只是选择器，语义上不属于注册表。
- **解析时机与位置**：CLI 入口（main 包）在启动时获取 `git --version` 输出之后、
  `puller.Get(opts.Mode)` 之前完成 auto → 实际模式的解析并回写 `opts.Mode`。
  必须在 `Get` 之前，否则 `Get("auto")` 报 "unknown mode"。
- **复用现有基建，零新增底层函数**：版本解析与能力判定直接使用 gitutil 包现有的
  `ParseGitVersion`（兼容 "x.y.z.windows.1"、"-rc1" 等后缀）和 `SupportsSparseCheckoutCone`（≥ 2.25）。
- **决策规则**：
  - 解析成功且 ≥ 2.25 → `snip`，日志形如 "auto: git x.y.z ≥ 2.25, 使用 snip 模式"
  - 解析成功但 < 2.25 → `full`，日志说明版本不足、已降级
  - 解析失败 → `full` + 告警（保守策略：full 对任意 Git 版本可用；
    即便误判，snip 内部 `checkGitVersion` 还有第二道拦截）
- **决策逻辑抽为 main 包纯函数**（输入 `git --version` 输出串，返回模式名 + 说明），
  供 main 调用并单独表驱动测试。这是 main 包第一个可独立单测的纯函数。
- **帮助文本与模式列表**：
  - `-mode` flag 描述在动态列表基础上显式包含 `auto` 及一句话说明（auto 检测 Git 版本自动选 snip/full）
  - `-list-modes` 输出补充 auto 条目及其语义描述，与注册模式的展示并存
- **默认值不变**：`-mode` 默认仍为 `full`，auto 为 opt-in。
- **缓存行为**：解析后 `opts.Mode` 已是 "snip"/"full"，后续缓存 key 与显式指定该模式完全一致，无需任何缓存层改动。
- **无 schema / API / 依赖变更**：纯标准库 + 现有内部包，无 go.mod 变化。

## 测试决策

- **好测试标准**：只测外部可观察行为——给定版本输出串，决策函数返回预期模式名；
  不 mock git 进程、不测内部实现细节。
- **决策纯函数表驱动测试（main 包）**，覆盖：
  - `git version 2.39.5` → snip；`git version 2.25.0` → snip（边界，含）
  - `git version 2.24.4` → full（边界，不含）；`git version 2.20.1` → full
  - `git version 2.30.1.windows.1` → snip（后缀兼容）
  - `git version 2.32.0-rc1` → snip（预发布后缀兼容）
  - 空串 / 乱串 / 缺 minor 的版本串 → full（解析失败保守路径）
  - `git version 3.0.0` → snip（主版本跨越）
- **测试先例**：gitutil 包中 `ParseGitVersion`/`SupportsSparseCheckoutCone` 已有同类表驱动版本串测试，风格对齐。
- **端到端验证（可选，利用现有 Docker 多版本矩阵）**：在已有的低版本 Git 容器
  （alpine 3.9/3.11，git 2.20/2.24）中跑 `-mode auto` 端到端用例，断言日志显示选择 full 且拉取成功；
  高版本容器断言选择 snip。Docker 测试基建（Dockerfile.test / docker-compose.test.yml）已存在，仅需加用例。

## 范围之外

- **默认模式改为 auto**：保持 full 默认，避免存量流水线隐性行为变化（缓存 key 从 full 切到 snip）。待 auto 线上验证后再议。
- **运行时回退**（auto 选了 snip 后拉取失败自动降级 full 重跑）：snip 失败多为网络问题，回退 full 同样会失败，且会使重试/缓存编排复杂化。auto 只在启动时决策一次。
- **特性探测式检测**（实际执行 `git sparse-checkout` 子命令验证发行版裁剪）：与 snip 内部 `checkGitVersion` 行为保持一致（版本号判定），探测式留作后续优化。
- **`--mode` 之外的自动策略配置**（如偏好权重、禁用某模式的黑名单）：YAGNI。

## 其他备注

- 项目长期记忆中有一条约束："不能仅凭 `git --version` 判断特性可用性（发行版可能裁剪）"。
  auto 的版本号判定与 snip 现有内部检测粒度一致；若真遇到"版本号达标但特性被裁"的环境，
  用户可显式 `-mode full` 绕过，且该场景已列入上述"特性探测"后续优化项。
- `main_test.go` 目前只是占位说明（无可测纯函数），本次为其补充第一批真实单测，注释需同步更新。
