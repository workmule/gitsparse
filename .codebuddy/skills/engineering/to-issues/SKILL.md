---
name: to-issues
description: 将计划、规格或 PRD 拆分为可独立领取的 Issue，使用 tracer-bullet 垂直切片，保存为本地文档。当用户想把计划转为 issue、创建实现工单、或拆解工作时使用。
---

# 拆分为 Issue

将计划拆分为可独立领取的 issue，使用垂直切片（tracer bullet）。

Issue 只保存为本地 Markdown 文档，不提交到远端 issue tracker。

## 流程

### 1. 收集上下文

从对话上下文中已有的内容开始。如果用户传递了 issue 引用（issue 编号、URL 或路径）作为参数，从本地文档获取并阅读其完整正文和评论。

### 2. 探索代码库（可选）

如果你尚未探索过代码库，去了解代码的当前状态。Issue 标题和描述应使用项目的领域术语表词汇，并尊重你涉及区域的 ADR。

### 3. 起草垂直切片

将计划拆分为 **tracer bullet** issue。每个 issue 是一个端到端贯穿所有集成层的薄垂直切片，而不是某一层的水平切片。

切片可以是"HITL（需人参与）"或"AFK（可自动完成）"。HITL 切片需要人类交互，例如架构决策或设计评审。AFK 切片可以在没有人类交互的情况下实现和合并。尽可能优先 AFK。

<vertical-slice-rules>
- 每个切片交付一条窄但完整的路径，贯穿每一层（schema、API、UI、测试）
- 一个完成的切片可以独立演示或验证
- 优先多个薄切片而非少数厚切片
</vertical-slice-rules>

### 4. 向用户确认

以编号列表呈现拆分方案。对每个切片，展示：

- **标题**：简短描述性名称
- **类型**：HITL / AFK
- **被阻塞于**：哪些其他切片（如有）必须先完成
- **覆盖的用户故事**：这解决了哪些用户故事（如果源材料中有的话）

向用户确认：

- 粒度感觉对吗？（太粗 / 太细）
- 依赖关系正确吗？
- 有没有切片需要合并或进一步拆分？
- HITL 和 AFK 标记正确吗？

迭代直到用户批准拆分方案。

### 5. 保存 Issue 到本地文档

对每个批准的切片，使用下面的 issue 正文模板，保存为本地 Markdown 文档到 `docs/issues/` 目录。

**文件命名格式**：`docs/issues/<slice-number>-<prd-name>-<slice-name>.md`

- `<slice-number>`：两位数字序号（01、02、...）
- `<prd-name>`：来源 PRD 的名称（去掉 `prd-` 前缀和 `.md` 后缀），如 PRD 文件为 `prd-playerinfo-sync.md`，则 prd-name 为 `playerinfo-sync`
- `<slice-name>`：切片简短描述（kebab-case）

示例：`docs/issues/01-playerinfo-sync-proto-filter.md`

按依赖顺序保存 issue（阻塞者优先），这样你可以在"被阻塞于"字段中引用真实的 issue 文件名。

<issue-template>
## 父级 Issue

对 issue tracker 上父 issue 的引用（如果来源是现有 issue，否则省略此节）。

## 要构建什么

对这个垂直切片的简洁描述。描述端到端行为，而非逐层实现。

**禁止**：
- 不要写具体文件路径（如 `account.go`、`service.go`），它们很快会过时
- 不要写逐层实现细节（如"在 xxx 函数中新增 xxx 逻辑"）
- 不要把验收标准写成代码审查 checklist（如"xxx.go 新增 xxx 字段"）

**允许的例外**：协议定义、schema、类型形状等决策密集的代码片段可以内联，因为它们比散文更精确表达决策。只保留决策密集的部分——不是可运行的 demo，只是重要的部分。

每个切片应在「要构建什么」末尾包含一句话描述「一个完成的切片：...」，说明该切片完成后可以如何独立演示或验证。

## 验收标准

以行为验证为主，描述"完成后能观察到什么"，而非"改了哪些文件"。

- [ ] 行为标准 1（如：客户端上报后 Account 中能读取到缓存的 PlayerInfo）
- [ ] 行为标准 2（如：未上报时进入返回错误码）
- [ ] 行为标准 3（如：go build 编译通过）

## 被阻塞于

- 对阻塞工单的引用（如有）

或"无 - 可立即开始"（如果无阻塞）。

</issue-template>

不要关闭或修改任何父 issue。
