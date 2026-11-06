package adapter

// Adapter 装配：配置 → 设备 id → 描述符表 → 自动发现 → Launcher/Backend → SessionManager → WS 客户端。
// 根 ctx 由 main 的 signal.NotifyContext 逐层透传（K8），业务层不使用 context.Background()。

import (
	"context"
	"errors"
	"log/slog"
	"runtime"

	"github.com/agenthub/agenthub/internal/config"
)

// Adapter 是设备侧适配器的顶层对象。
type Adapter struct {
	cfg        *config.AdapterConfig
	version    string
	deviceID   string
	deviceName string

	registry   *Registry
	discoverer *Discoverer
	launcher   *Launcher
	backend    Backend
	manager    *SessionManager
	ws         *WSClient
}

// New 完成全部装配；rootCtx 用于派生每个 CLI turn 的子 ctx。
func New(rootCtx context.Context, cfg *config.AdapterConfig, version string) (*Adapter, error) {
	if cfg == nil {
		return nil, errors.New("适配器配置不能为空")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	deviceID, err := LoadOrCreateDeviceID(cfg)
	if err != nil {
		return nil, err
	}

	registry := NewRegistry()
	discoverer := NewDiscoverer(cfg, registry)
	launcher := NewLauncher(registry)
	backend := NewBackend(registry, launcher)
	wsClient := NewWSClient(cfg, version, deviceID, discoverer)
	manager := NewSessionManager(rootCtx, backend, wsClient, discoverer, registry,
		cfg.TurnTimeout(), cfg.WorkingDirectory())
	wsClient.AttachManager(manager)

	return &Adapter{
		cfg:        cfg,
		version:    version,
		deviceID:   deviceID,
		deviceName: cfg.GetDeviceName(),
		registry:   registry,
		discoverer: discoverer,
		launcher:   launcher,
		backend:    backend,
		manager:    manager,
		ws:         wsClient,
	}, nil
}

// Run 阻塞运行：启动即探测 → 周期重探测 → WS 主循环 → 退出时 cancel 全部活跃 turn。
func (a *Adapter) Run(ctx context.Context) error {
	// 启动即探测（阻塞到产出首版快照才发 device.register，C-12 / Q13）。
	snapshot := a.discoverer.Probe(ctx)
	slog.Info("适配器启动",
		"version", a.version,
		"device_id", a.deviceID,
		"device_name", a.deviceName,
		"os", runtime.GOOS,
		"arch", runtime.GOARCH,
		"agents_found", len(snapshot),
		"platform", config.RedactURL(a.cfg.PlatformWSURL), // 脱敏：不打印完整 WS URL / token
	)
	for _, agent := range snapshot {
		// R-17 结构化启动日志：发现到哪些 agent。
		slog.Info("发现 agent",
			"type", agent.Type,
			"display_name", agent.DisplayName,
			"version", agent.Version,
			"status", agent.Status,
		)
	}

	// 每 2min 重探测并向平台重发全量快照对账（单向只增不减由 Discoverer 保证）。
	go a.discoverer.Loop(ctx, a.cfg.DiscoverPeriod(), a.ws.SendSnapshot)

	err := a.ws.Run(ctx)
	a.manager.CancelAll()
	if errors.Is(err, context.Canceled) {
		slog.Info("适配器已优雅退出", "device_id", a.deviceID)
		return nil
	}
	return err
}

// DeviceID 返回本机持久化设备 id（供联调与测试断言）。
func (a *Adapter) DeviceID() string {
	return a.deviceID
}

// Discoverer 返回自动发现器（供联调与测试断言）。
func (a *Adapter) Discoverer() *Discoverer {
	return a.discoverer
}
