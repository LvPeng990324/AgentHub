package config

// QA 补充测试：配置加载的必填校验、默认值、非法取值与「错误信息必须是可读中文」。
// 依据：§11.3 配置项 + 系统设计 §5.3（DB 路径父目录自建）+ R-01（密码不可明文出现在日志）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeJSON 把对象写入临时配置文件并返回路径。
func writeJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("序列化测试配置失败: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}
	return path
}

// validServerMap 返回一份合法 server 段，供各用例按需改字段。
func validServerMap() map[string]any {
	return map[string]any{
		"password":                 "qa-password",
		"domain":                   "qa.local",
		"listen_addr":              "127.0.0.1:8080",
		"device_token":             "qa-device-token",
		"db_path":                  "./agenthub.db",
		"token_ttl_seconds":        86400,
		"heartbeat_timeout_cycles": 3,
		"log_level":                "info",
		"protocol_version":         1,
	}
}

// TestQAConfigMissingRequiredFields 缺少必填字段必须给出含字段名的简体中文错误。
func TestQAConfigMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]any)
		wantSub string
	}{
		{"缺 password", func(m map[string]any) { delete(m, "password") }, "server.password"},
		{"空 password", func(m map[string]any) { m["password"] = "   " }, "server.password"},
		{"缺 domain", func(m map[string]any) { delete(m, "domain") }, "server.domain"},
		{"缺 device_token", func(m map[string]any) { delete(m, "device_token") }, "server.device_token"},
		{"空 listen_addr 回落默认值", func(m map[string]any) { m["listen_addr"] = "" }, ""},
		{"token_ttl 为负", func(m map[string]any) { m["token_ttl_seconds"] = -1 }, "token_ttl_seconds"},
		{"心跳周期为负", func(m map[string]any) { m["heartbeat_timeout_cycles"] = -3 }, "heartbeat_timeout_cycles"},
		{"协议版本不匹配", func(m map[string]any) { m["protocol_version"] = 2 }, "protocol_version"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := validServerMap()
			testCase.mutate(server)
			path := writeJSON(t, map[string]any{"server": server})
			cfg, err := LoadServerConfig(path)
			if testCase.wantSub == "" {
				if err != nil {
					t.Fatalf("应加载成功，实际报错: %v", err)
				}
				if cfg.ListenAddr != DefaultListenAddr {
					t.Errorf("空 listen_addr 应回落默认值 %s，实际 %s", DefaultListenAddr, cfg.ListenAddr)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s 应校验失败，实际通过", testCase.name)
			}
			if !strings.Contains(err.Error(), testCase.wantSub) {
				t.Errorf("错误信息应包含 %q，实际 %q", testCase.wantSub, err.Error())
			}
			if !containsChinese(err.Error()) {
				t.Errorf("配置错误必须是简体中文，实际 %q", err.Error())
			}
		})
	}
}

// containsChinese 粗判字符串是否含中日韩统一表意文字（用于断言"中文错误提示"）。
func containsChinese(text string) bool {
	for _, char := range text {
		if char >= 0x4E00 && char <= 0x9FFF {
			return true
		}
	}
	return false
}

// TestQAConfigMergedAndMalformed 验证合并配置取段逻辑与非法 JSON 的中文错误。
func TestQAConfigMergedAndMalformed(t *testing.T) {
	// 合并配置：server 段可被正确取出。
	path := writeJSON(t, map[string]any{
		"server":  validServerMap(),
		"adapter": map[string]any{"platform_ws_url": "wss://qa.local/api/v1/ws/adapter", "device_token": "qa-device-token"},
	})
	cfg, err := LoadServerConfig(path)
	if err != nil {
		t.Fatalf("合并配置应加载成功: %v", err)
	}
	if cfg.Password != "qa-password" || cfg.DeviceToken != "qa-device-token" {
		t.Errorf("合并配置取段错误: %+v", cfg)
	}
	if cfg.PasswordHash != HashHex("qa-password") || cfg.DeviceTokenHash != HashHex("qa-device-token") {
		t.Error("密码/设备令牌哈希未按 SHA-256 计算")
	}
	if cfg.VerifyPassword("qa-password") != true {
		t.Error("正确密码应校验通过")
	}
	if cfg.VerifyPassword("qa-password ") {
		t.Error("密码比较不得忽略尾部空白（必须严格相等）")
	}
	if cfg.VerifyDeviceToken("") {
		t.Error("空设备令牌必须校验失败")
	}

	// 非法 JSON。
	badPath := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badPath, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("写入非法配置失败: %v", err)
	}
	if _, err := LoadServerConfig(badPath); err == nil {
		t.Error("非法 JSON 应报错")
	} else if !containsChinese(err.Error()) {
		t.Errorf("非法 JSON 错误应为中文，实际 %q", err.Error())
	}

	// 文件不存在。
	if _, err := LoadServerConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("配置文件不存在应报错")
	} else if !containsChinese(err.Error()) {
		t.Errorf("文件不存在错误应为中文，实际 %q", err.Error())
	}

	// 空路径。
	if _, err := LoadServerConfig("   "); err == nil {
		t.Error("空配置路径应报错")
	}
}

// TestQAAdapterConfigRequiredFields 适配器侧必填校验：平台地址与设备令牌。
func TestQAAdapterConfigRequiredFields(t *testing.T) {
	cases := []struct {
		name    string
		adapter map[string]any
		wantSub string
	}{
		{"合法", map[string]any{"platform_ws_url": "wss://qa.local/api/v1/ws/adapter", "device_token": "tok"}, ""},
		{"缺平台地址", map[string]any{"device_token": "tok"}, "platform_ws_url"},
		{"缺设备令牌", map[string]any{"platform_ws_url": "wss://qa.local/api/v1/ws/adapter"}, "device_token"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := writeJSON(t, map[string]any{"adapter": testCase.adapter})
			cfg, err := LoadAdapterConfig(path)
			if testCase.wantSub == "" {
				if err != nil {
					t.Fatalf("应加载成功: %v", err)
				}
				if cfg.DiscoverPeriodSeconds != DefaultDiscoverPeriodSeconds {
					t.Errorf("默认发现周期应为 %d，实际 %d", DefaultDiscoverPeriodSeconds, cfg.DiscoverPeriodSeconds)
				}
				if cfg.HeartbeatIntervalSeconds != DefaultHeartbeatIntervalSeconds {
					t.Errorf("默认心跳周期应为 %d，实际 %d", DefaultHeartbeatIntervalSeconds, cfg.HeartbeatIntervalSeconds)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s 应校验失败", testCase.name)
			}
			if !strings.Contains(err.Error(), testCase.wantSub) {
				t.Errorf("错误信息应包含 %q，实际 %q", testCase.wantSub, err.Error())
			}
			if !containsChinese(err.Error()) {
				t.Errorf("适配器配置错误应为中文，实际 %q", err.Error())
			}
		})
	}
}

// TestQARedactURLCoversToken URL 脱敏必须去掉 query（K7：token 不能落日志）。
func TestQARedactURLCoversToken(t *testing.T) {
	cases := map[string]string{
		"wss://agent-hub.example.com/api/v1/ws/adapter?token=secret-token&device_id=dev_1": "wss://agent-hub.example.com/api/v1/ws/adapter",
		"http://127.0.0.1:8080/api/v1/ws/client?token=abc":                                 "http://127.0.0.1:8080/api/v1/ws/client",
	}
	for input, want := range cases {
		got := RedactURL(input)
		if got != want {
			t.Errorf("RedactURL(%q) 期望 %q，实际 %q", input, want, got)
		}
		if strings.Contains(got, "token") || strings.Contains(got, "secret") {
			t.Errorf("脱敏后仍含敏感信息: %q", got)
		}
	}
}
