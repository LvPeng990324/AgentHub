package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// handleMeta 处理 E04 GET /api/v1/meta（公开）。
// 只返回 name / version / domain / protocol_version / agent_types，
// 绝不含登录密码、设备令牌、监听地址（U5 / C-11）。
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeOK(w, MetaData{
		Name:            "AgentHub",
		Version:         s.version,
		Domain:          s.cfg.Domain,
		ProtocolVersion: protocol.ProtocolVersion,
		AgentTypes:      protocol.AgentTypes(),
	})
}

// handleHealth 处理 E05 GET /api/v1/health（公开）。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		slog.Error("健康检查：数据库不可用", "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	writeOK(w, HealthData{
		Status:  "ok",
		UptimeS: int64(time.Since(s.startedAt).Seconds()),
		DB:      "ok",
	})
}

// handleMe 处理 E03 GET /api/v1/auth/me（Bearer）。
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	_, expiresAt, ok := tokenFromContext(r.Context())
	if !ok {
		writeFail(w, protocol.CodeUnauthorized)
		return
	}
	writeOK(w, MeData{Authenticated: true, ExpiresAt: expiresAt})
}
