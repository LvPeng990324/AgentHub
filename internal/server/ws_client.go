package server

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// clientUpgrader 用于浏览器侧通道。登录 token 放在 query（浏览器 WS 不能自定义 header）；
// 非 Cookie 鉴权，无 CSRF 风险，故不限制 Origin。
var clientUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// handleWSClient 处理 /api/v1/ws/client 握手。
//
// 鉴权只接受**登录 token**（auth_tokens 表哈希 + 未过期），设备令牌永远不能用于本通道（D-2）。
// 失败在 Upgrade 之前返回 HTTP 401；成功后立即下行 F14 client.ready（§7.1）。
func (s *Server) handleWSClient(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	_, code := s.auth.Validate(r.Context(), token)
	if code != protocol.CodeOK {
		slog.Warn("浏览器 WS 握手失败：登录态无效", "token", maskToken(token), "code", code)
		writeFail(w, code)
		return
	}

	ws, err := clientUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("浏览器 WS Upgrade 失败", "err", err)
		return
	}
	conn := NewWSConn(
		"cli_"+uuid.NewString(), "", ConnKindClient,
		ws, s.hub, s.dispatchClientFrame, s.rootCtx,
	)
	s.hub.Register(conn)
	conn.Start()
	s.hub.SendToConn(conn, protocol.FrameClientReady, protocol.ClientReadyPayload{
		ServerTime:      nowUTC(),
		ProtocolVersion: protocol.ProtocolVersion,
	})
	slog.Info("浏览器 WS 已连接", "conn_id", conn.ID)
}

// dispatchClientFrame 分派浏览器入站帧；本函数不持有 Hub 锁（K9）。
func (s *Server) dispatchClientFrame(ctx context.Context, conn *WSConn, message protocol.Message) {
	switch message.Type {
	case protocol.FrameChatSend:
		var payload protocol.ChatSendPayload
		if err := message.DecodePayload(&payload); err != nil {
			slog.Warn("解析 chat.send 失败", "err", err)
			return
		}
		s.chat.OnClientSend(ctx, conn, payload)
	case protocol.FrameChatStop:
		var payload protocol.ChatStopPayload
		if err := message.DecodePayload(&payload); err != nil {
			slog.Warn("解析 chat.stop 失败", "err", err)
			return
		}
		s.chat.Stop(ctx, payload)
	default:
		slog.Warn("client 通道收到不支持的帧", "type", message.Type, "conn_id", conn.ID)
	}
}
