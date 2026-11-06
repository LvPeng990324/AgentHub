// Command agenthub-server 是平台服务单二进制入口：
// 加载配置 → 打开并迁移 SQLite → 装配 Store / Repo / Auth / Hub / Chat / REST+WS 路由 → 启动 HTTP → 优雅关闭。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/agenthub/agenthub/internal/config"
	"github.com/agenthub/agenthub/internal/server"
)

// version 由 Makefile 通过 -ldflags "-X main.version=..." 注入。
var version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "平台服务异常:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.json", "平台服务配置文件路径")
	flag.Parse()

	cfg, err := config.LoadServerConfig(*configPath)
	if err != nil {
		return err
	}
	slog.SetDefault(config.NewLogger(cfg.LogLevel))

	// main 是唯一允许创建根 context 的位置；所有业务层逐层透传该 ctx（K8）。
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(rootCtx, cfg, version)
	if err != nil {
		return err
	}
	if err := srv.Run(rootCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	slog.Info("平台服务已停止")
	return nil
}
