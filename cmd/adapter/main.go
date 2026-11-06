// Command agenthub-adapter 是每台设备上的连接适配器入口：
// 加载配置 → 读/生成 device_id → 自动发现 → WS 客户端 → Backend/SessionManager → 优雅退出。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/agenthub/agenthub/internal/adapter"
	"github.com/agenthub/agenthub/internal/config"
)

// version 由 Makefile 通过 -ldflags "-X main.version=..." 注入。
var version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "适配器异常:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "adapter.json", "适配器配置文件路径")
	flag.Parse()

	cfg, err := config.LoadAdapterConfig(*configPath)
	if err != nil {
		return err
	}
	slog.SetDefault(config.NewLogger(cfg.LogLevel))
	// 启动日志显式打印 working_dir（K7：路径可打，令牌/完整 WS URL 不可打）。
	// 排查「Agent 启动失败」时第一时间就能确认 CLI 在哪个目录下运行。
	slog.Info("适配器配置已加载",
		"platform", config.RedactURL(cfg.PlatformWSURL),
		"device_name", cfg.GetDeviceName(),
		"working_dir", cfg.WorkingDirectory(),
		"turn_timeout_s", int(cfg.TurnTimeout().Seconds()),
	)

	// main 是唯一允许创建根 context 的位置；其余组件逐层透传该 ctx（K8）。
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	deviceAdapter, err := adapter.New(rootCtx, cfg, version)
	if err != nil {
		return err
	}
	return deviceAdapter.Run(rootCtx)
}
