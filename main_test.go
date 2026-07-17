package main

// main 包的测试见:
//   - internal/gitutil/gitutil_test.go  (公共工具函数)
//   - internal/puller/fullpull/fullpull_test.go (全量拉取模式集成)
//
// main.go 本身只含 CLI 解析 + 调用 puller.Pull(), 无可独立单测的纯函数.
// 如需端到端测试, 使用 `make run` 配合真实仓库.
