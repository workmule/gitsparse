# gitsparse - git sparse 目录拉取工具
# 用法: make run REPO=... REF=... DIRS=...

# 测试仓库 (从大到小):
#   大:   REPO=https://github.com/pytorch/pytorch.git REF=main DIRS=torch
#   大:   REPO=https://github.com/microsoft/vscode.git REF=main DIRS=src/vs
#   中:   REPO=https://github.com/numpy/numpy.git    REF=main DIRS=numpy
#   LFS:  REPO=https://github.com/godotengine/godot.git REF=master DIRS=core
REPO ?= https://github.com/numpy/numpy.git
REF ?= main
DIRS ?= numpy
OUTPUT ?= ./output
# 超时 (每个网络操作, 0=不限)
TIMEOUT ?= 1m
# 重试次数 (超时或失败后自动重试)
RETRIES ?= 3
# 缓存 TTL (超过此时间的缓存自动清理, 0=不清理)
CACHE_TTL ?= 24h
# 跳过缓存 (force fresh clone)
NO_CACHE ?= false

# 工具路径 (允许外部覆盖; 系统未安装时对应目标会给出提示)
GO        ?= go
GOFMT     ?= gofmt
GOLINT    ?= golangci-lint

# 扫描时是否自动修复 (make check FIX=true 会跑 gofmt -w / goimports -w)
FIX ?= false

# 构建产物
BIN ?= gitsparse

# 快速测试 (含 LFS 自动检测 + 缓存复用)
run:
	@go run . -repo "$(REPO)" -ref "$(REF)" -dirs "$(DIRS)" -output "$(OUTPUT)" \
		-timeout "$(TIMEOUT)" -retries $(RETRIES) \
		-cache-ttl "$(CACHE_TTL)" -no-cache=$(NO_CACHE)

# ============================================================================
# 代码扫描检查 (build 前置依赖)
# ============================================================================

# gofmt: 检查代码格式是否规范 (FIX=true 时自动修复)
.PHONY: fmt
fmt:
ifeq ($(FIX),true)
	@echo ">> [fmt] 格式化代码 (gofmt -w)"
	@$(GOFMT) -s -w .
else
	@echo ">> [fmt] 检查代码格式 (gofmt -l)"
	@files=$$($(GOFMT) -s -l . 2>/dev/null); \
	if [ -n "$$files" ]; then \
		echo "以下文件未通过 gofmt 检查 (运行 make fmt FIX=true 修复):"; \
		echo "$$files"; \
		exit 1; \
	fi
endif

# go vet: 编译器级静态检查 (可疑代码结构, printf 参数, 锁拷贝等)
.PHONY: vet
vet:
	@echo ">> [vet] go vet 静态检查"
	@$(GO) vet ./...

# golangci-lint: 综合静态分析 (配置见 .golangci.yml)
# 未安装时给出提示而非失败, 避免 CI 环境无该工具时阻塞
.PHONY: lint
lint:
	@command -v $(GOLINT) >/dev/null 2>&1 || { \
		echo ">> [lint] 跳过: $(GOLINT) 未安装 (安装: https://golangci-lint.run/usage/install/)"; \
		exit 0; \
	}
	@echo ">> [lint] golangci-lint 综合检查"
	@$(GOLINT) run ./...

# go mod: 检查依赖是否整洁 (无未使用/缺失依赖)
# 注意: 纯标准库项目无 go.sum, 检查需容错
.PHONY: mod-tidy-check
mod-tidy-check:
	@echo ">> [mod] 检查 go.mod/go.sum 是否整洁"
	@cp go.mod go.mod.bak; [ -f go.sum ] && cp go.sum go.sum.bak || true; \
	$(GO) mod tidy; \
	if ! diff -q go.mod go.mod.bak >/dev/null; then \
		echo "go.mod 不整洁, 请运行 'go mod tidy' 后提交"; \
		mv go.mod.bak go.mod; [ -f go.sum.bak ] && mv go.sum.bak go.sum || true; \
		exit 1; \
	fi; \
	if [ -f go.sum.bak ] && ! diff -q go.sum go.sum.bak >/dev/null; then \
		echo "go.sum 不整洁, 请运行 'go mod tidy' 后提交"; \
		mv go.mod.bak go.mod; mv go.sum.bak go.sum; \
		exit 1; \
	fi; \
	rm -f go.mod.bak go.sum.bak

# check: 聚合所有扫描检查 (build 的前置依赖)
.PHONY: check
check: fmt vet lint mod-tidy-check
	@echo ">> [check] 所有扫描检查通过"

# 构建二进制 (前置: check)
.PHONY: build
build: check
	@echo ">> [build] 编译 $(BIN)"
	@$(GO) build -o $(BIN) .

# 运行测试 (前置: check)
.PHONY: test
test: check
	@$(GO) test -v -timeout 60s .

# 快速测试 (跳过网络/集成测试)
.PHONY: test-short
test-short:
	@$(GO) test -v -short -timeout 60s .

# 清理输出
.PHONY: clean
clean:
	@rm -rf $(OUTPUT) $(BIN)

# 清理所有缓存
.PHONY: clean-cache
clean-cache:
	@rm -rf /tmp/gitsparse-cache

.PHONY: run test test-short clean clean-cache
