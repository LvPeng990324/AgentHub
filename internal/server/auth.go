package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/agenthub/agenthub/internal/config"
	"github.com/agenthub/agenthub/pkg/protocol"
)

// ErrInvalidPassword 是密码校验失败的哨兵错误；Handler 据此映射 1002。
var ErrInvalidPassword = errors.New("密码错误")

// AuthService 负责登录密码校验、不透明 token 签发/校验/吊销，以及设备令牌校验。
//
// 两套凭据彻底分离（D-2）：
//   - 登录 token：base64url(crypto/rand 32B)，DB 只存 SHA-256 哈希 + 过期时间；
//   - 设备令牌：仅用于 /api/v1/ws/adapter 握手，永远不能登录 REST 或 ws/client。
type AuthService struct {
	store *Store
	cfg   *config.ServerConfig
}

// NewAuthService 构造鉴权服务。
func NewAuthService(store *Store, cfg *config.ServerConfig) *AuthService {
	return &AuthService{store: store, cfg: cfg}
}

// Login 校验密码并签发一个 24h（可配置）不透明 token。
func (a *AuthService) Login(ctx context.Context, password string) (LoginData, error) {
	if !a.cfg.VerifyPassword(password) {
		return LoginData{}, ErrInvalidPassword
	}
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return LoginData{}, fmt.Errorf("生成登录 token 失败: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buffer)
	now := time.Now().UTC()
	createdAt := now.Format(time.RFC3339)
	expiresAt := now.Add(a.cfg.TokenTTL()).Format(time.RFC3339)

	// 只落哈希，绝不落明文 token（§6.2）。
	if _, err := a.store.DB.ExecContext(ctx,
		`INSERT INTO auth_tokens(token_hash, created_at, expires_at) VALUES(?, ?, ?)`,
		config.HashHex(token), createdAt, expiresAt); err != nil {
		return LoginData{}, fmt.Errorf("持久化登录 token 哈希失败: %w", err)
	}
	return LoginData{
		Token:     token,
		TokenType: "Bearer",
		ExpiresIn: a.cfg.TokenTTLSeconds,
	}, nil
}

// Validate 校验登录 token：返回过期时间与错误码（0 表示有效）。
// 过期 token 会顺手删除哈希，避免表无限增长。
func (a *AuthService) Validate(ctx context.Context, token string) (string, int) {
	if token == "" {
		return "", protocol.CodeUnauthorized
	}
	hash := config.HashHex(token)
	var expiresAt string
	err := a.store.DB.QueryRowContext(ctx,
		`SELECT expires_at FROM auth_tokens WHERE token_hash = ?`, hash).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", protocol.CodeUnauthorized
	}
	if err != nil {
		return "", protocol.CodeInternalError
	}
	expiry, parseErr := time.Parse(time.RFC3339, expiresAt)
	if parseErr != nil || time.Now().UTC().After(expiry) {
		if _, delErr := a.store.DB.ExecContext(ctx,
			`DELETE FROM auth_tokens WHERE token_hash = ?`, hash); delErr != nil {
			return "", protocol.CodeInternalError
		}
		return "", protocol.CodeTokenExpired
	}
	return expiresAt, protocol.CodeOK
}

// Logout 删除 token 哈希，使登录态即时失效。
func (a *AuthService) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if _, err := a.store.DB.ExecContext(ctx,
		`DELETE FROM auth_tokens WHERE token_hash = ?`, config.HashHex(token)); err != nil {
		return fmt.Errorf("吊销登录 token 失败: %w", err)
	}
	return nil
}

// ValidateDeviceToken 校验设备令牌（仅用于 WS 握手，REST 一律不调用）。
func (a *AuthService) ValidateDeviceToken(token string) bool {
	return a.cfg.VerifyDeviceToken(token)
}

// PruneExpired 清理已过期 token 哈希；进程启动时调用一次。
func (a *AuthService) PruneExpired(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := a.store.DB.ExecContext(ctx,
		`DELETE FROM auth_tokens WHERE expires_at < ?`, now); err != nil {
		return fmt.Errorf("清理过期登录 token 失败: %w", err)
	}
	return nil
}
