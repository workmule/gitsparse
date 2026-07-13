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
TIMEOUT ?= 5m
# 重试次数 (超时或失败后自动重试)
RETRIES ?= 3
# 缓存 TTL (超过此时间的缓存自动清理, 0=不清理)
CACHE_TTL ?= 24h
# 跳过缓存 (force fresh clone)
NO_CACHE ?= false

# 快速测试 (含 LFS 自动检测 + 缓存复用)
run:
	@go run . -repo "$(REPO)" -ref "$(REF)" -dirs "$(DIRS)" -output "$(OUTPUT)" \
		-timeout "$(TIMEOUT)" -retries $(RETRIES) \
		-cache-ttl "$(CACHE_TTL)" -no-cache $(NO_CACHE)

# 构建二进制
build:
	@go build -o gitsparse .

# 运行测试
test:
	@go test -v -timeout 60s .

# 快速测试 (跳过网络/集成测试)
test-short:
	@go test -v -short -timeout 60s .

# 清理输出
clean:
	@rm -rf $(OUTPUT) gitsparse

# 清理所有缓存
clean-cache:
	@rm -rf /tmp/gitsparse-cache

.PHONY: run build test test-short clean clean-cache
