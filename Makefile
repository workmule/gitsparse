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

# 扫描时是否自动修复 (默认 true: gofmt -w / goimports -w)
FIX ?= true

# 构建产物
BIN ?= gitsparse

# ============================================================================
# 版本号规范 (必须遵守)
# ============================================================================
# 版本号格式: v<大版本>.<次版本>.<修订号>.<构建时间戳 YYYYMMDDHHMMSS>
# 当前大版本号固定为 1, 只允许 v1.x.x.x, 不许调整大版本号!
# 次版本/修订号: make bump 自动 +1 (LEVEL=patch/minor)
# 最后一段构建时间戳由 make install 自动替换
# version-check 会校验 main.go 中 Version 常量, 违规直接失败
VERSION_MAJOR ?= 1

# 校验 main.go 版本号符合 v$(VERSION_MAJOR).x.x.x 规范 (大版本号不许调整)
.PHONY: version-check
version-check:
	@echo ">> [version] 检查版本号规范 (v$(VERSION_MAJOR).x.x.x, 大版本号固定)"
	@if ! grep -qE '^const Version = "v$(VERSION_MAJOR)\.[0-9]+\.[0-9]+\.[0-9]+"' main.go; then \
		echo "版本号违规: main.go 中 Version 必须为 v$(VERSION_MAJOR).x.x.x 格式"; \
		echo "  当前值: $$(grep '^const Version' main.go)"; \
		echo "  规范: 大版本号固定为 $(VERSION_MAJOR), 不许调整"; \
		exit 1; \
	fi

# 自动递增版本号 (前置 version-check, 保证格式合法后才改):
#   make bump             -> 修订号 +1           (v1.2.3.t -> v1.2.4.t)
#   make bump LEVEL=minor -> 次版本 +1, 修订清零  (v1.2.3.t -> v1.3.0.t)
# 大版本号与最后一段时间戳不动 (时间戳由 make install 替换)
LEVEL ?= patch

.PHONY: bump
bump: version-check
	@cur=$$(sed -n 's/^const Version = "\(v[0-9.]*\)"/\1/p' main.go); \
	minor=$$(echo "$$cur" | cut -d. -f2); patch=$$(echo "$$cur" | cut -d. -f3); \
	case "$(LEVEL)" in \
		patch)  patch=$$((patch+1));; \
		minor)  minor=$$((minor+1)); patch=0;; \
		*)      echo ">> [bump] 失败: LEVEL 仅支持 patch/minor (当前: $(LEVEL))"; exit 1;; \
	esac; \
	sed -i -E 's/^(const Version = "v)[0-9]+\.[0-9]+\.[0-9]+(\.[0-9]+")/\1$(VERSION_MAJOR).'"$$minor.$$patch"'\2/' main.go; \
	echo ">> [bump] $$cur -> v$(VERSION_MAJOR).$$minor.$$patch.$$(echo "$$cur" | cut -d. -f4)"

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
check: fmt vet lint mod-tidy-check version-check
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

# 安装到 $GOBIN (或 $GOPATH/bin), 之后可直接 gitsparse ... 调用
# 构建时把 main.go 的版本号最后一段替换为构建时刻 (YYYYMMDDHHMMSS), 便于区分构建版本
# 前置 version-check: 防止大版本号被误改后继续安装
.PHONY: install
install: version-check
	@$(eval BUILD_VER := $(shell date +%Y%m%d%H%M%S))
	@sed -i -E 's/^(const Version = "v[0-9]+\.[0-9]+\.[0-9]+\.)([0-9]+)"/\1$(BUILD_VER)"/' main.go
	@echo ">> [install] go install (版本号末位改为 $(BUILD_VER))"
	@$(GO) install .


.PHONY: all
all: build

# ============================================================================
# Docker 多 Git 版本测试
# =============================================================================

# 在指定 alpine 版本 (对应不同 Git 版本) 的容器中运行测试
# 用法: make test-docker ALPINE=3.9
#       make test-docker-all  (跑全部 3 个版本)
ALPINE ?= 3.12

.PHONY: test-docker
test-docker:
	@echo ">> [docker] 构建并测试 (alpine $(ALPINE))"
	@docker build --build-arg ALPINE_VERSION=$(ALPINE) -t gitsparse-test:alpine$(ALPINE) -f Dockerfile.test .

.PHONY: test-docker-all
test-docker-all:
	@echo ">> [docker] 并行测试多个 Git 版本"
	@docker-compose -f docker-compose.test.yml up --abort-on-container-exit

.PHONY: test-docker-clean
test-docker-clean:
	@docker-compose -f docker-compose.test.yml down --rmi local 2>/dev/null || true
	@docker rmi gitsparse-test:alpine3.9 gitsparse-test:alpine3.11 gitsparse-test:alpine3.12 2>/dev/null || true

.PHONY: run test test-short clean clean-cache install version-check bump test-docker test-docker-all test-docker-clean
