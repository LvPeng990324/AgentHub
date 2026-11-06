# AgentHub · 完整构建入口（T05）
# 全部 Go 产物统一 CGO_ENABLED=0（C-2 / K13）；前端用 npm（非 pnpm）。
# 交叉编译矩阵固定 5 个产物到 bin/；make build 顺序严格 web → go build（不可颠倒）。
# 日志统一英文，避免 Windows 控制台把 UTF-8 中文按本地代码页显示成乱码。

VERSION      ?= 0.1.0
GO           ?= go
NPM          ?= npm
CGO_ENABLED  := 0
LDFLAGS      := -s -w -X main.version=$(VERSION)
BIN_DIR      := bin
NPM_CACHE    := .npm-cache
GOPROXY      ?= https://goproxy.cn,direct
export GOPROXY

SERVER_TARGETS := \
	$(BIN_DIR)/agenthub-server-linux-amd64 \
	$(BIN_DIR)/agenthub-server-linux-arm64 \
	$(BIN_DIR)/agenthub-server-windows-amd64.exe
ADAPTER_TARGETS := \
	$(BIN_DIR)/agenthub-adapter-linux-amd64 \
	$(BIN_DIR)/agenthub-adapter-windows-amd64.exe
ALL_TARGETS := $(SERVER_TARGETS) $(ADAPTER_TARGETS)

.PHONY: all web build release server adapter dev-web dev-server tidy fmt vet test clean smoke verify placeholder check-web
# 交叉编译产物强制重建（发布构建不接受「目标文件已存在」的增量跳过，
# 否则源码/前端更新后 bin/ 仍是旧二进制）。
.PHONY: $(ALL_TARGETS)

# 交付前置校验（D-10）：web/dist 必须是真实 SPA，不能是占位页。
# 所有 bin/* 交叉编译都先过这个检查，任何「先 placeholder 再编 bin/」的误操作都会立即失败。
check-web:
	@bash scripts/check-web.sh

all: build

# ---------------------------------------------------------------------------
# 前端：独立 npm 缓存目录规避全局缓存损坏；npm ci 保证 lock 可复现
# ---------------------------------------------------------------------------
web:
	@echo "[web] 1/2 npm ci (isolated cache web/.npm-cache)"
	cd web && $(NPM) ci --cache $(NPM_CACHE)
	@echo "[web] 2/2 npm run build -> web/dist (real SPA)"
	cd web && $(NPM) run build
	@echo "[web] PASS: real SPA generated"

# ---------------------------------------------------------------------------
# 本机快速构建（调试用，不是发布产物）
# ---------------------------------------------------------------------------
server:
	@echo "[server] build native server binary"
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/agenthub-server ./cmd/server
	@echo "[server] PASS: $(BIN_DIR)/agenthub-server"

adapter:
	@echo "[adapter] build native adapter binary"
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/agenthub-adapter ./cmd/adapter
	@echo "[adapter] PASS: $(BIN_DIR)/agenthub-adapter"

# ---------------------------------------------------------------------------
# 交叉编译矩阵（§11.2）：纯 Go 依赖，无需目标平台 C 工具链
# ---------------------------------------------------------------------------
$(BIN_DIR)/agenthub-server-linux-amd64: | check-web
	@echo "[build] server linux/amd64"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $@ ./cmd/server

$(BIN_DIR)/agenthub-server-linux-arm64: | check-web
	@echo "[build] server linux/arm64"
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $@ ./cmd/server

$(BIN_DIR)/agenthub-server-windows-amd64.exe: | check-web
	@echo "[build] server windows/amd64"
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $@ ./cmd/server

$(BIN_DIR)/agenthub-adapter-linux-amd64: | check-web
	@echo "[build] adapter linux/amd64 (also for WSL)"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $@ ./cmd/adapter

$(BIN_DIR)/agenthub-adapter-windows-amd64.exe: | check-web
	@echo "[build] adapter windows/amd64"
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $@ ./cmd/adapter

# ★顺序铁律：先 make web 生成真实 SPA，再 go build 内嵌；否则会 embed 到占位页。
build:
	@echo "[build] step 1/2: build frontend"
	@$(MAKE) web
	@echo "[build] step 2/2: cross-compile 5 CGO_ENABLED=0 artifacts"
	@$(MAKE) $(ALL_TARGETS)
	@echo "[build] PASS: 5 artifacts generated, all embedding the real SPA"
	@ls -lh $(ALL_TARGETS)

# ★发版专用：固定顺序 web → 交叉编译（与 build 等价，语义更明确、可单独调用）。
# 任一 bin/* 目标都内置 check-web 前置校验：前端缺失会立即失败，不会产出内嵌占位页的坏包。
release:
	@echo "[release] 发版专用：先构建前端再交叉编译（顺序不可颠倒）"
	@$(MAKE) web
	@$(MAKE) $(ALL_TARGETS)
	@echo "[release] PASS: 5 artifacts embed the real SPA"

# ---------------------------------------------------------------------------
# 质量门禁
# ---------------------------------------------------------------------------
vet:
	@echo "[vet] CGO_ENABLED=0 go vet ./..."
	CGO_ENABLED=$(CGO_ENABLED) $(GO) vet ./...
	@echo "[vet] PASS"

test:
	@echo "[test] CGO_ENABLED=0 go test ./... -count=1"
	CGO_ENABLED=$(CGO_ENABLED) $(GO) test ./... -count=1
	@echo "[test] PASS"

tidy:
	@echo "[tidy] go mod tidy"
	CGO_ENABLED=$(CGO_ENABLED) $(GO) mod tidy
	@echo "[tidy] PASS"

fmt:
	@echo "[fmt] go fmt ./..."
	$(GO) fmt ./...
	@echo "[fmt] PASS"

verify: vet test
	@echo "[verify] npm run typecheck"
	cd web && $(NPM) run typecheck
	@echo "[verify] PASS: vet + test + typecheck"

# ---------------------------------------------------------------------------
# 开发期
# ---------------------------------------------------------------------------
dev-web:
	@echo "[dev-web] Vite :5173, /api proxied to 127.0.0.1:8080 (WS enabled)"
	cd web && $(NPM) run dev

dev-server:
	@echo "[dev-server] go run ./cmd/server -config config.json"
	CGO_ENABLED=$(CGO_ENABLED) $(GO) run ./cmd/server -config config.json

smoke:
	@echo "[smoke] run end-to-end smoke test"
	bash scripts/smoke.sh

# ---------------------------------------------------------------------------
# 恢复占位页：仅用于恢复仓库默认态（clone 后未 make web 时 go build 可编、页面有友好提示）。
# ★ 发版前不要执行 —— 占位页会被 go:embed 打进二进制。发版请用 make release（内置前置校验）。
# ---------------------------------------------------------------------------
placeholder:
	@echo "[placeholder] restore web/dist/index.html placeholder"
	bash scripts/restore-placeholder.sh

clean:
	@echo "[clean] remove bin/ and web/dist build artifacts (keep placeholder)"
	rm -rf $(BIN_DIR)
	find web/dist -mindepth 1 ! -name index.html -exec rm -rf {} +
	@$(MAKE) placeholder
	@echo "[clean] PASS: repository default state restored"
