# zero-memcp 构建与检查入口
#
# 这里只有一条设计原则：**快慢分开**。
#
# 常规测试要能在十几秒内给出答案，否则人会跳过它；性能测试要能重复
# 跑出可比的数字，否则它只是个摆设。两者混在一起的结果是两边都做不到：
# 一条跑 17 秒的启动测试会把每次提交拖慢，而人一旦开始习惯「等一会儿」，
# 下一个真故障就会被当成常态。

GO      ?= go
BIN     := bin/zero-memcp
PKG     := ./cmd/zero-memcp
PERF_TAG := perf

.PHONY: help
help:
	@echo "make test      常规测试（秒级，不含 perf 标签的慢测试）"
	@echo "make perf      性能测试 + 基准（十几秒到一分钟）"
	@echo "make bench     只跑 Benchmark"
	@echo "make check     vet + 常规测试（提交前跑这个）"
	@echo "make all       全量：vet + 常规测试 + 性能测试"
	@echo "make build     本机构建（纯 Go 静态二进制）"
	@echo "make build-onnx  带 ONNX 引擎（需 libonnxruntime）"
	@echo "make cross     六平台交叉编译（仅默认入口，无 CGO）"

.PHONY: test
test:
	$(GO) test ./... -count=1

# 性能测试单独一条命令：它们要跑十几秒到一分钟，不该混进日常迭代。
# 用构建标签而不是 -short 隔离：-short 只能把所有慢测试一起关掉，
# 而标签能让「哪些是性能测试」成为文件属性，看得见。
.PHONY: perf
perf:
	$(GO) test -tags $(PERF_TAG) ./... -count=1 -v -timeout 20m

.PHONY: bench
bench:
	$(GO) test -tags $(PERF_TAG) ./... -run xxx -bench . -benchtime 2s -count=1

.PHONY: check
check: vet test

.PHONY: all
all: vet test perf

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: build
build:
	CGO_ENABLED=0 $(GO) build -o $(BIN) $(PKG)
	@$(BIN) version

# ONNX 入口：唯一一个「代码本身有部署代价」的引擎，要链接
# libonnxruntime、交叉编译要各平台备库，所以单独一条命令。
.PHONY: build-onnx
build-onnx:
	CGO_ENABLED=1 $(GO) build -tags onnx -o $(BIN)-onnx $(PKG)
	@$(BIN)-onnx version

# 默认入口没有 CGO，因此六平台可以在同一台机器上并发交叉编译。
.PHONY: cross
cross:
	@mkdir -p dist
	@for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do \
		os=$${t%/*}; arch=$${t#*/}; ext=""; \
		[ "$$os" = "windows" ] && ext=".exe"; \
		echo "  $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -ldflags="-s -w" \
			-o dist/zero-memcp-$$os-$$arch$$ext $(PKG) || exit 1; \
	done
	@ls -lh dist/

.PHONY: clean
clean:
	rm -rf bin dist
