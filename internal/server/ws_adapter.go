package server

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// adapterUpgrader 用于设备侧通道。鉴权靠设备令牌（非 Cookie），不存在 CSRF 风险，
// 因此不限制 Origin；公网边界由 nginx 终止 TLS/WSS。
var adapterUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// handleWSAdapter 处理 /api/v1/ws/adapter 握手（F01~F04 / F07~F09 所在通道）。
//
// 鉴权：query `token` 优先、`Authorization: Bearer` 兜底；两者都只接受**设备令牌**，
// 永远不接受登录 token（D-2）。校验失败在 Upgrade 之前返回 HTTP 401，不发 WS 帧。
func (s *Server) handleWSAdapter(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	token := query.Get("token")
	if token == "" {
		token = bearerToken(r)
	}
	if !s.auth.ValidateDeviceToken(token) {
		slog.Warn("设备 WS 握手失败：设备令牌无效", "token", maskToken(token))
		writeFail(w, protocol.CodeAdapterUnauthorized)
		return
	}
	if rawVersion := query.Get("protocol_version"); rawVersion != "" {
		version, err := strconv.Atoi(rawVersion)
		if err != nil || version != protocol.ProtocolVersion {
			slog.Warn("设备 WS 握手失败：协议版本不匹配", "protocol_version", rawVersion)
			writeFail(w, protocol.CodeProtocolVersionMismatch)
			return
		}
	}
	deviceID := query.Get("device_id")
	if deviceID == "" {
		writeFail(w, protocol.CodeBadRequest)
		return
	}

	ws, err := adapterUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("设备 WS Upgrade 失败", "device_id", deviceID, "err", err)
		return
	}
	conn := NewWSConn(
		"adp_"+uuid.NewString(), deviceID, ConnKindAdapter,
		ws, s.hub, s.dispatchAdapterFrame, s.rootCtx,
	)
	s.hub.Register(conn)
	conn.Start()
	slog.Info("设备 WS 已连接", "device_id", deviceID, "conn_id", conn.ID)

	// §7.1：device.register_ack 推迟到收到 F01 device.register 之后再下发。
}

// dispatchAdapterFrame 分派设备侧入站帧；本函数不持有 Hub 锁（K9）。
func (s *Server) dispatchAdapterFrame(ctx context.Context, conn *WSConn, message protocol.Message) {
	switch message.Type {
	case protocol.FrameDeviceRegister:
		s.onDeviceRegister(ctx, conn, message)
	case protocol.FrameDeviceHeartbeat:
		s.onDeviceHeartbeat(ctx, conn, message)
	case protocol.FrameChatChunk:
		var payload protocol.ChatChunkPayload
		if err := message.DecodePayload(&payload); err != nil {
			slog.Warn("解析 chat.chunk 失败", "err", err)
			return
		}
		s.chat.OnAdapterChunk(ctx, payload)
	case protocol.FrameChatDone:
		var payload protocol.ChatDonePayload
		if err := message.DecodePayload(&payload); err != nil {
			slog.Warn("解析 chat.done 失败", "err", err)
			return
		}
		s.chat.OnAdapterDone(ctx, payload)
	case protocol.FrameChatError:
		var payload protocol.ChatErrorPayload
		if err := message.DecodePayload(&payload); err != nil {
			slog.Warn("解析 chat.error 失败", "err", err)
			return
		}
		s.chat.OnAdapterError(ctx, payload)
	default:
		slog.Warn("adapter 通道收到不支持的帧", "type", message.Type, "device_id", conn.DeviceID)
	}
}

// onDeviceRegister 处理 F01 全量快照，随后下发 F02 device.register_ack。
func (s *Server) onDeviceRegister(ctx context.Context, conn *WSConn, message protocol.Message) {
	var payload protocol.DeviceRegisterPayload
	if err := message.DecodePayload(&payload); err != nil {
		slog.Warn("解析 device.register 失败", "err", err)
		return
	}
	if payload.DeviceID == "" || payload.DeviceID != conn.DeviceID {
		slog.Warn("device.register 的 device_id 与握手不一致，拒绝",
			"handshake_device_id", conn.DeviceID, "frame_device_id", payload.DeviceID)
		return
	}
	if payload.ProtocolVersion != 0 && payload.ProtocolVersion != protocol.ProtocolVersion {
		slog.Warn("device.register 的协议版本不匹配，拒绝",
			"device_id", conn.DeviceID, "protocol_version", payload.ProtocolVersion)
		return
	}
	registeredIDs, err := s.hub.OnDeviceRegister(ctx, payload)
	if err != nil {
		slog.Error("处理设备全量快照失败", "device_id", conn.DeviceID, "err", err)
		return
	}
	s.hub.SendToDevice(conn.DeviceID, protocol.FrameDeviceRegisterAck,
		protocol.DeviceRegisterAckPayload{
			DeviceID:           conn.DeviceID,
			Accepted:           true,
			ServerVersion:      s.version,
			ProtocolVersion:    protocol.ProtocolVersion,
			HeartbeatIntervalS: protocol.HeartbeatIntervalSeconds,
			RegisteredAgentIDs: registeredIDs,
		})
}

// onDeviceHeartbeat 处理 F03 心跳，随后下发 F04 heartbeat_ack。
func (s *Server) onDeviceHeartbeat(ctx context.Context, conn *WSConn, message protocol.Message) {
	var payload protocol.HeartbeatPayload
	if err := message.DecodePayload(&payload); err != nil {
		slog.Warn("解析 device.heartbeat 失败", "err", err)
		return
	}
	if payload.DeviceID == "" {
		payload.DeviceID = conn.DeviceID
	}
	if payload.DeviceID != conn.DeviceID {
		slog.Warn("device.heartbeat 的 device_id 与握手不一致，拒绝",
			"handshake_device_id", conn.DeviceID, "frame_device_id", payload.DeviceID)
		return
	}
	types := make([]string, 0, len(payload.Agents))
	for _, agent := range payload.Agents {
		if agent.Type != "" {
			types = append(types, agent.Type)
		}
	}
	if err := s.hub.OnDeviceHeartbeat(ctx, conn.DeviceID, types); err != nil {
		slog.Warn("处理设备心跳失败", "device_id", conn.DeviceID, "err", err)
		return
	}
	s.hub.SendToDevice(conn.DeviceID, protocol.FrameHeartbeatAck,
		protocol.HeartbeatAckPayload{ServerTime: nowUTC()})
}
