// Package config 负责平台服务与设备侧适配器的 JSON 配置加载、默认值填充与必填校验。
// 两份配置可各自单独成文，也可按 §11.3 合并为一个同时含 server / adapter 段的文件。
package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// ServerConfig 对应 config.json 的 server 段（或整个文件本身）。
// Password / DeviceToken 仅在服务端配置文件与进程内存中出现，绝不进入前端与日志（C-11 / K7）。
type ServerConfig struct {
	Password               string `json:"password"`
	Domain                 string `json:"domain"`
	ListenAddr             string `json:"listen_addr"`
	DeviceToken            string `json:"device_token"`
	DBPath                 string `json:"db_path"`
	TokenTTLSeconds        int    `json:"token_ttl_seconds"`
	HeartbeatTimeoutCycles int    `json:"heartbeat_timeout_cycles"`
	LogLevel               string `json:"log_level"`
	ProtocolVersion        int    `json:"protocol_version"`

	// 派生字段：进程内保存 SHA-256 十六进制摘要，用于握手表侧比对与日志脱敏展示。
	PasswordHash    string `json:"-"`
	DeviceTokenHash string `json:"-"`
}

// 服务端默认值（§11.3）。
const (
	DefaultListenAddr             = "127.0.0.1:8080"
	DefaultDBPath                 = "./agenthub.db"
	DefaultTokenTTLSeconds        = 86400
	DefaultHeartbeatTimeoutCycles = 3
	DefaultLogLevel               = "info"
)

// configEnvelope 用于兼容「合并配置」与「单侧配置」两种文件形态。
type configEnvelope struct {
	Server  json.RawMessage `json:"server"`
	Adapter json.RawMessage `json:"adapter"`
}

// LoadServerConfig 读取配置文件、填充默认值、校验必填项并返回可用配置。
// 校验失败一律返回明确的简体中文错误。
func LoadServerConfig(path string) (*ServerConfig, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("配置文件路径不能为空")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取平台配置文件失败: %w", err)
	}
	target, err := selectSection(data, true)
	if err != nil {
		return nil, err
	}
	var cfg ServerConfig
	if err := json.Unmarshal(target, &cfg); err != nil {
		return nil, fmt.Errorf("解析 platform/server 配置失败: %w", err)
	}
	applyServerDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.PasswordHash = HashHex(cfg.Password)
	cfg.DeviceTokenHash = HashHex(cfg.DeviceToken)
	return &cfg, nil
}

// selectSection 若文件含 server / adapter 顶层段则取对应段，否则把整个文件当作单侧配置。
func selectSection(data []byte, wantServer bool) ([]byte, error) {
	var envelope configEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("解析配置文件 JSON 失败: %w", err)
	}
	section := envelope.Adapter
	if wantServer {
		section = envelope.Server
	}
	if len(section) > 0 && string(section) != "null" {
		return section, nil
	}
	return data, nil
}

// applyServerDefaults 为零值字段填充 §11.3 默认值。
func applyServerDefaults(cfg *ServerConfig) {
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		cfg.ListenAddr = DefaultListenAddr
	}
	if strings.TrimSpace(cfg.DBPath) == "" {
		cfg.DBPath = DefaultDBPath
	}
	if cfg.TokenTTLSeconds == 0 {
		cfg.TokenTTLSeconds = DefaultTokenTTLSeconds
	}
	if cfg.HeartbeatTimeoutCycles == 0 {
		cfg.HeartbeatTimeoutCycles = DefaultHeartbeatTimeoutCycles
	}
	if strings.TrimSpace(cfg.LogLevel) == "" {
		cfg.LogLevel = DefaultLogLevel
	}
	if cfg.ProtocolVersion == 0 {
		cfg.ProtocolVersion = protocol.ProtocolVersion
	}
}

// Validate 校验平台侧必填字段与取值域。
func (c *ServerConfig) Validate() error {
	if strings.TrimSpace(c.Password) == "" {
		return errors.New("配置项 server.password 不能为空，请设置登录密码")
	}
	if strings.TrimSpace(c.Domain) == "" {
		return errors.New("配置项 server.domain 不能为空，请设置绑定域名")
	}
	if strings.TrimSpace(c.DeviceToken) == "" {
		return errors.New("配置项 server.device_token 不能为空，请设置设备令牌")
	}
	if strings.TrimSpace(c.ListenAddr) == "" {
		return errors.New("配置项 server.listen_addr 不能为空")
	}
	if strings.TrimSpace(c.DBPath) == "" {
		return errors.New("配置项 server.db_path 不能为空")
	}
	if c.TokenTTLSeconds <= 0 {
		return errors.New("配置项 server.token_ttl_seconds 必须大于 0")
	}
	if c.HeartbeatTimeoutCycles <= 0 {
		return errors.New("配置项 server.heartbeat_timeout_cycles 必须大于 0")
	}
	if c.ProtocolVersion != protocol.ProtocolVersion {
		return fmt.Errorf("配置项 server.protocol_version 必须为 %d，当前为 %d", protocol.ProtocolVersion, c.ProtocolVersion)
	}
	return nil
}

// TokenTTL 返回登录 token 的有效期。
func (c *ServerConfig) TokenTTL() time.Duration {
	return time.Duration(c.TokenTTLSeconds) * time.Second
}

// HeartbeatTimeout 返回离线判定阈值（默认 3×15s=45s）。
func (c *ServerConfig) HeartbeatTimeout() time.Duration {
	return time.Duration(c.HeartbeatTimeoutCycles*protocol.HeartbeatIntervalSeconds) * time.Second
}

// VerifyPassword 用定长 SHA-256 摘要做常数时间比较，防止时序攻击（§6.2）。
func (c *ServerConfig) VerifyPassword(password string) bool {
	return subtle.ConstantTimeCompare([]byte(HashHex(password)), []byte(c.PasswordHash)) == 1
}

// VerifyDeviceToken 校验设备令牌（与登录密码彻底分离，仅用于 ws/adapter 握手）。
func (c *ServerConfig) VerifyDeviceToken(token string) bool {
	if strings.TrimSpace(token) == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(HashHex(token)), []byte(c.DeviceTokenHash)) == 1
}

// HashHex 返回字符串的 SHA-256 十六进制摘要（token 落库与令牌比对的统一入口）。
func HashHex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// ParseLogLevel 把配置中的日志级别字符串映射为 slog.Level，未知值回退 info。
func ParseLogLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// NewLogger 构造结构化 JSON 日志器（K7）。调用方仍须遵守脱敏铁律：
// 不打印 password / device_token / token 全文、prompt 全文与完整 WS URL。
func NewLogger(raw string) *slog.Logger {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: ParseLogLevel(raw)})
	return slog.New(handler)
}

// RedactURL 去掉 URL 的 query / userinfo / fragment，仅保留 scheme + host + path。
// 用于日志中打印平台 WS 地址时不泄露 query 中的 token（K7 / §7.1）。
func RedactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	parsed.RawQuery = ""
	parsed.User = nil
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String()
}
