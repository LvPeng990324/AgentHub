package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenthub/agenthub/internal/config"
	"github.com/agenthub/agenthub/internal/server"
	"github.com/agenthub/agenthub/pkg/protocol"
)

type devicesPayload struct {
	Data struct {
		Total int `json:"total"`
		Items []struct {
			ID         string  `json:"id"`
			Online     bool    `json:"online"`
			LastSeenAt *string `json:"last_seen_at"`
			Agents     []struct {
				ID string `json:"id"`
			} `json:"agents"`
		} `json:"items"`
	} `json:"data"`
}

func fetchDevices(t *testing.T, baseURL string, token string) devicesPayload {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/devices", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("请求 /devices 失败: %v", err)
	}
	defer response.Body.Close()
	var payload devicesPayload
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("解析 /devices 失败: %v", err)
	}
	return payload
}

func loginForIntegration(t *testing.T, baseURL string) string {
	t.Helper()
	response, err := http.Post(
		baseURL+"/api/v1/auth/login",
		"application/json",
		strings.NewReader(`{"password":"test-login-password"}`),
	)
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	defer response.Body.Close()
	var envelope struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("解析登录响应失败: %v", err)
	}
	if envelope.Data.Token == "" {
		t.Fatal("登录未返回 token")
	}
	return envelope.Data.Token
}

// TestAdapterRealServerIntegration：真 server + 真 adapter WS 客户端联调。
// 覆盖 F01 全量快照落库、F02 ack 心跳参数解析、F03 心跳刷新、断开重连后快照对账收敛。
func TestAdapterRealServerIntegration(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Password:               "test-login-password",
		Domain:                 "localhost",
		ListenAddr:             "127.0.0.1:0",
		DeviceToken:            "test-device-token",
		DBPath:                 filepath.Join(t.TempDir(), "integration.db"),
		TokenTTLSeconds:        3600,
		HeartbeatTimeoutCycles: 3,
		LogLevel:               "error",
		ProtocolVersion:        protocol.ProtocolVersion,
	}
	serverCfg.PasswordHash = config.HashHex(serverCfg.Password)
	serverCfg.DeviceTokenHash = config.HashHex(serverCfg.DeviceToken)
	platform, err := server.New(context.Background(), serverCfg, "0.1.0-test")
	if err != nil {
		t.Fatalf("启动真实平台服务失败: %v", err)
	}
	defer func() { _ = platform.Shutdown(context.Background()) }()
	httpServer := httptest.NewServer(platform.Handler())
	defer httpServer.Close()
	token := loginForIntegration(t, httpServer.URL)

	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/api/v1/ws/adapter"
	adapterCfg := &config.AdapterConfig{
		PlatformWSURL:            wsURL,
		DeviceToken:              "test-device-token",
		DeviceName:               "集成测试设备",
		DiscoverPeriodSeconds:    120,
		HeartbeatIntervalSeconds: 1, // 加速心跳验证；F02 ack 仍下发生产值 15
		DeviceIDPath:             filepath.Join(t.TempDir(), "device_id"),
		AgentPaths:               map[string]string{},
		LogLevel:                 "error",
	}
	registry := NewRegistry()
	discoverer := NewDiscoverer(adapterCfg, registry)
	discoverer.mu.Lock()
	discoverer.agents[protocol.AgentTypeClaude] = &discoveredState{
		agent: DiscoveredAgent{
			Type: protocol.AgentTypeClaude, DisplayName: "Claude Code",
			Version: "9.9.9", ExecPath: "stub", Status: protocol.AgentStatusAvailable,
		},
	}
	discoverer.mu.Unlock()

	const deviceID = "dev_integration"
	client := NewWSClient(adapterCfg, "0.1.0-test", deviceID, discoverer)
	runCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- client.Run(runCtx) }()

	// ① F01 全量快照落库：平台 GET /devices 能看到设备与其 agent。
	waitFor(t, 12*time.Second, func() bool {
		payload := fetchDevices(t, httpServer.URL, token)
		for _, item := range payload.Data.Items {
			if item.ID == deviceID && len(item.Agents) == 1 &&
				item.Agents[0].ID == deviceID+":"+protocol.AgentTypeClaude {
				return true
			}
		}
		return false
	}, "平台未收到 F01 全量快照（devices 中缺少设备或 agent）")

	// ② F02 register_ack 下发的心跳参数被解析。
	waitFor(t, 5*time.Second, func() bool {
		client.mu.RLock()
		defer client.mu.RUnlock()
		return client.heartbeatSecond == protocol.HeartbeatIntervalSeconds
	}, "未正确解析 F02 register_ack 的 heartbeat_interval_s")

	// ③ F03 心跳刷新 last_seen_at（1s 加速间隔；平台默认 15s）。
	readLastSeen := func() string {
		for _, item := range fetchDevices(t, httpServer.URL, token).Data.Items {
			if item.ID == deviceID && item.LastSeenAt != nil {
				return *item.LastSeenAt
			}
		}
		return ""
	}
	before := readLastSeen()
	waitFor(t, 8*time.Second, func() bool {
		after := readLastSeen()
		return after != "" && after != before
	}, "F03 心跳未刷新 last_seen_at")

	// ④ 断开重连：同一 device_id 重发 F01，平台以快照幂等 upsert 对账，状态收敛一致。
	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("WS 客户端未在 ctx 取消后退出")
	}
	reconnectClient := NewWSClient(adapterCfg, "0.1.0-test", deviceID, discoverer)
	reconnectCtx, cancelReconnect := context.WithCancel(context.Background())
	reconnectDone := make(chan error, 1)
	go func() { reconnectDone <- reconnectClient.Run(reconnectCtx) }()
	waitFor(t, 12*time.Second, func() bool {
		payload := fetchDevices(t, httpServer.URL, token)
		if payload.Data.Total != 1 || len(payload.Data.Items) != 1 {
			return false
		}
		item := payload.Data.Items[0]
		return item.ID == deviceID && item.Online && len(item.Agents) == 1
	}, "重连后平台状态未收敛（应仍为 1 台设备 / 1 个 agent / 在线）")

	cancelReconnect()
	select {
	case <-reconnectDone:
	case <-time.After(5 * time.Second):
		t.Fatal("重连客户端未退出")
	}
}
