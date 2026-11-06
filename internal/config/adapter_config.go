package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// AdapterConfig 对应 adapter.json 的 adapter 段（或整个文件本身）。
type AdapterConfig struct {
	PlatformWSURL            string            `json:"platform_ws_url"`
	DeviceToken              string            `json:"device_token"`
	DeviceName               string            `json:"device_name"`
	DiscoverPeriodSeconds    int               `json:"discover_period_seconds"`
	HeartbeatIntervalSeconds int               `json:"heartbeat_interval_seconds"`
	DeviceIDPath             string            `json:"device_id_path"`
	AgentPaths               map[string]string `json:"agent_paths"`
	LogLevel                 string            `json:"log_level"`
	// TurnTimeoutSeconds 是单轮 CLI 生成的硬超时（秒）。用指针区分两种语义：
	// nil（配置项缺失）= 默认 1800；显式 0 = 禁用超时；正数 = 该秒数。
	// 超时后适配器 KillTree 回收整树并回 chat.error{5005}，避免 CLI 挂死导致该 agent 长期不可用（K8）。
	TurnTimeoutSeconds *int `json:"turn_timeout_s"`
	// WorkingDir 是 CLI 的工作目录（同时作为 ACP session/new 的 cwd 与子进程 cmd.Dir）。
	// 空值 → 适配器进程当前目录；相对路径 → 转绝对；必须存在且是目录（否则启动即失败）。
	// ★ 必须非空：kimi 等真实 ACP CLI 会校验 workspace root，空 cwd 会直接拒绝 session/new。
	WorkingDir string `json:"working_dir"`
}

// 适配器侧默认值（§11.3）。
const (
	DefaultDiscoverPeriodSeconds    = 120
	DefaultHeartbeatIntervalSeconds = 15
	DefaultDeviceIDPath             = "~/.agenthub/device_id"
	DefaultTurnTimeoutSeconds       = 1800
)

// LoadAdapterConfig 读取配置文件、填充默认值、校验必填项并返回可用配置。
func LoadAdapterConfig(path string) (*AdapterConfig, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("配置文件路径不能为空")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取适配器配置文件失败: %w", err)
	}
	target, err := selectSection(data, false)
	if err != nil {
		return nil, err
	}
	var cfg AdapterConfig
	if err := unmarshalStrict(target, &cfg); err != nil {
		return nil, fmt.Errorf("解析 adapter 配置失败: %w", err)
	}
	applyAdapterDefaults(&cfg)
	// working_dir 需要文件系统校验（必须是存在的目录），失败即拒绝启动。
	if err := cfg.resolveWorkingDir(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// resolveWorkingDir 规范化 working_dir：空值取进程 cwd、展开 ~、相对路径转绝对，
// 并校验目标必须是存在的目录。失败返回中文错误（适配器启动阶段直接拒绝）。
func (c *AdapterConfig) resolveWorkingDir() error {
	dir := strings.TrimSpace(c.WorkingDir)
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("获取适配器当前工作目录失败: %w", err)
		}
		dir = cwd
	}
	absolute, err := filepath.Abs(expandHomeDir(dir))
	if err != nil {
		return fmt.Errorf("解析配置项 adapter.working_dir 失败: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("配置项 adapter.working_dir 指向的目录不存在：%s", absolute)
	}
	c.WorkingDir = absolute
	return nil
}

// WorkingDirectory 返回 CLI 工作目录；未经 Load 规范化时回退到进程当前目录，
// 保证 ACP 的 cwd 永远不为空（kimi 会校验 workspace root）。
func (c *AdapterConfig) WorkingDirectory() string {
	if strings.TrimSpace(c.WorkingDir) != "" {
		return c.WorkingDir
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return ""
}

// expandHomeDir 展开以 ~ 开头的路径（供 working_dir 与 device_id_path 共用）。
func expandHomeDir(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~/"), "~\\"))
	}
	return path
}

// unmarshalStrict 单独抽出以便未来在不改变调用点的前提下增强校验。
func unmarshalStrict(data []byte, target *AdapterConfig) error {
	if len(data) == 0 {
		return errors.New("配置内容为空")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return err
	}
	return nil
}

func applyAdapterDefaults(cfg *AdapterConfig) {
	if cfg.DiscoverPeriodSeconds == 0 {
		cfg.DiscoverPeriodSeconds = DefaultDiscoverPeriodSeconds
	}
	if cfg.HeartbeatIntervalSeconds == 0 {
		cfg.HeartbeatIntervalSeconds = DefaultHeartbeatIntervalSeconds
	}
	if strings.TrimSpace(cfg.DeviceIDPath) == "" {
		cfg.DeviceIDPath = DefaultDeviceIDPath
	}
	if strings.TrimSpace(cfg.LogLevel) == "" {
		cfg.LogLevel = DefaultLogLevel
	}
	if cfg.AgentPaths == nil {
		cfg.AgentPaths = make(map[string]string)
	}
	if cfg.TurnTimeoutSeconds == nil {
		defaultTimeout := DefaultTurnTimeoutSeconds
		cfg.TurnTimeoutSeconds = &defaultTimeout
	}
}

// Validate 校验适配器侧必填字段与取值域。
func (c *AdapterConfig) Validate() error {
	if strings.TrimSpace(c.PlatformWSURL) == "" {
		return errors.New("配置项 adapter.platform_ws_url 不能为空，请设置平台 WebSocket 地址")
	}
	if strings.TrimSpace(c.DeviceToken) == "" {
		return errors.New("配置项 adapter.device_token 不能为空，请填写与 server.device_token 一致的设备令牌")
	}
	if c.DiscoverPeriodSeconds <= 0 {
		return errors.New("配置项 adapter.discover_period_seconds 必须大于 0")
	}
	if c.HeartbeatIntervalSeconds <= 0 {
		return errors.New("配置项 adapter.heartbeat_interval_seconds 必须大于 0")
	}
	if strings.TrimSpace(c.DeviceIDPath) == "" {
		return errors.New("配置项 adapter.device_id_path 不能为空")
	}
	if c.TurnTimeoutSeconds != nil && *c.TurnTimeoutSeconds < 0 {
		return errors.New("配置项 adapter.turn_timeout_s 不能为负数（0 表示禁用单轮超时）")
	}
	return nil
}

// TurnTimeout 返回单轮硬超时；0 表示禁用（由用户显式配置 0）。
func (c *AdapterConfig) TurnTimeout() time.Duration {
	if c.TurnTimeoutSeconds == nil {
		return time.Duration(DefaultTurnTimeoutSeconds) * time.Second
	}
	if *c.TurnTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(*c.TurnTimeoutSeconds) * time.Second
}

// GetDeviceName 返回设备名；配置留空时自动取 os.Hostname()。
func (c *AdapterConfig) GetDeviceName() string {
	if strings.TrimSpace(c.DeviceName) != "" {
		return c.DeviceName
	}
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "unknown-device"
	}
	return hostname
}

// DiscoverPeriod 返回重探测周期（默认 2 分钟）。
func (c *AdapterConfig) DiscoverPeriod() time.Duration {
	return time.Duration(c.DiscoverPeriodSeconds) * time.Second
}

// HeartbeatInterval 返回心跳周期（默认 15s）。
func (c *AdapterConfig) HeartbeatInterval() time.Duration {
	return time.Duration(c.HeartbeatIntervalSeconds) * time.Second
}

// DeviceIDFile 把 device_id_path 中的 ~ 展开为用户主目录，返回绝对路径。
// 供 T04 的 device_id 持久化逻辑使用（文件本身不是会话数据，允许落盘）。
func (c *AdapterConfig) DeviceIDFile() (string, error) {
	path := expandHomeDir(c.DeviceIDPath)
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析 device_id 路径失败: %w", err)
	}
	return absolute, nil
}

// EffectiveHeartbeatSeconds 返回心跳秒数，供构造 WS URL 与退避计算时使用。
func (c *AdapterConfig) EffectiveHeartbeatSeconds() int {
	if c.HeartbeatIntervalSeconds <= 0 {
		return protocol.HeartbeatIntervalSeconds
	}
	return c.HeartbeatIntervalSeconds
}
