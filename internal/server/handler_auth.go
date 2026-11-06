package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// handleLogin 处理 E01 POST /api/v1/auth/login（公开）。
// 密码错误 → 1002；请求体非法 → 1004；服务异常 → 5000。日志不打印密码。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var request LoginRequest
	if err := decodeJSON(r, &request); err != nil {
		writeFail(w, protocol.CodeBadRequest)
		return
	}
	if strings.TrimSpace(request.Password) == "" {
		writeFail(w, protocol.CodeBadRequest)
		return
	}
	data, err := s.auth.Login(r.Context(), request.Password)
	if errors.Is(err, ErrInvalidPassword) {
		slog.Warn("登录失败：密码错误", "path", r.URL.Path)
		writeFail(w, protocol.CodeInvalidPassword)
		return
	}
	if err != nil {
		slog.Error("登录失败：服务内部错误", "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	writeOK(w, data)
}

// handleLogout 处理 E02 POST /api/v1/auth/logout（Bearer）；删除哈希后 token 即时失效。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	token, _, ok := tokenFromContext(r.Context())
	if !ok {
		writeFail(w, protocol.CodeUnauthorized)
		return
	}
	if err := s.auth.Logout(r.Context(), token); err != nil {
		slog.Error("登出失败", "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	writeOK(w, nil)
}
