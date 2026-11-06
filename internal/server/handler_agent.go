package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// handleListDiscovered 处理 E08 GET /api/v1/agents/discovered（Bearer），含未注册 agent。
func (s *Server) handleListDiscovered(w http.ResponseWriter, r *http.Request) {
	query := ParseListQuery(r)
	agents, total, err := s.agentRepo.List(r.Context(), AgentListFilter{Query: query})
	if err != nil {
		slog.Error("查询已发现 agent 失败", "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	writeOK(w, s.toAgentListData(agents, total))
}

// handleListRegistered 处理 E09 GET /api/v1/agents（Bearer），仅 registered=1。
func (s *Server) handleListRegistered(w http.ResponseWriter, r *http.Request) {
	query := ParseListQuery(r)
	agents, total, err := s.agentRepo.List(r.Context(), AgentListFilter{Query: query, RegisteredOnly: true})
	if err != nil {
		slog.Error("查询已注册 agent 失败", "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	writeOK(w, s.toAgentListData(agents, total))
}

// toAgentListData 把 Agent 行列表转换为统一列表 DTO（items 恒为非 null 空数组）。
func (s *Server) toAgentListData(agents []Agent, total int) AgentListData {
	items := make([]AgentDTO, 0, len(agents))
	for _, agent := range agents {
		items = append(items, ToAgentDTO(agent))
	}
	return AgentListData{Items: items, Total: total}
}

// handleRegister 处理 E10 POST /api/v1/agents/{agentId}/register（Bearer，幂等）。
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agentID := r.PathValue("agentId")
	agent, deviceID, err := s.agentRepo.RegisterOne(ctx, agentID)
	if errors.Is(err, ErrAgentNotFound) {
		writeFail(w, protocol.CodeAgentNotFound)
		return
	}
	if err != nil {
		slog.Error("注册 agent 失败", "agent_id", agentID, "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	// 可选 P1：通知适配器预热；适配器不处理也不影响全流程（U9）。
	s.hub.SendToDevice(deviceID, protocol.FrameAgentRegistered,
		protocol.AgentRegisteredPayload{AgentID: agentID})
	s.broadcastAgentStatus(ctx, agentID, protocol.ReasonRegistered)
	writeOK(w, ToAgentDTO(agent))
}

// handleUnregister 处理 E11 DELETE /api/v1/agents/{agentId}/register（Bearer）。
//
// D-6 铁律：DB 侧只由 AgentRepo.Unregister 在**单个事务**内完成三步；
// 本 Handler 在事务提交后负责 F12 设备通知、F11 状态广播与前端半截气泡兜底。
// 绝不删设备行 / agent 行、不断 WS、不清 remark，之后可重新注册（EnsureSession 会新建会话）。
func (s *Server) handleUnregister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agentID := r.PathValue("agentId")

	// 事务前读取在线态与会话 id，仅用于构造下行帧；不参与三态推断。
	before, err := s.agentRepo.GetByID(ctx, agentID)
	if errors.Is(err, ErrAgentNotFound) {
		writeFail(w, protocol.CodeAgentNotFound)
		return
	}
	if err != nil {
		slog.Error("取消注册前读取 agent 失败", "agent_id", agentID, "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	session, sessionErr := s.messageRepo.GetSessionByAgent(ctx, agentID)
	if sessionErr != nil && !errors.Is(sessionErr, ErrSessionNotFound) {
		slog.Error("取消注册前读取会话失败", "agent_id", agentID, "err", sessionErr)
		writeFail(w, protocol.CodeInternalError)
		return
	}

	// ★单事务三步：registered=0 → DELETE sessions（级联 messages）→ COMMIT
	deviceID, err := s.agentRepo.Unregister(ctx, agentID)
	if errors.Is(err, ErrAgentNotFound) {
		writeFail(w, protocol.CodeAgentNotFound)
		return
	}
	if err != nil {
		slog.Error("取消注册事务失败", "agent_id", agentID, "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}

	// 清内存流式缓冲（库里消息已被级联清空，无需再 UPDATE 占位）。
	s.chat.ForgetStreams(agentID)

	// ③ 向该设备 WS 推 F12 agent.unregistered
	s.hub.SendToDevice(deviceID, protocol.FrameAgentUnregistered,
		protocol.AgentUnregisteredPayload{AgentID: agentID})

	// ④ 广播 F11 agent.status（registered=false，online 保持独立）
	s.hub.BroadcastClients(protocol.FrameAgentStatus, protocol.AgentStatusPayload{
		Agents: []protocol.AgentStatusItem{{
			AgentID:    agentID,
			Online:     before.Online,
			Registered: false,
		}},
		Reason: protocol.ReasonUnregistered,
	})

	// ⑤ 若在对话页，下行 chat.done{interrupted} 兜底半截气泡（§9.4）。
	s.chat.BroadcastDone(protocol.ChatDonePayload{
		AgentID:      agentID,
		SessionID:    session.ID,
		AssistantSeq: 0,
		Status:       protocol.DoneStatusInterrupted,
	})

	writeOK(w, UnregisterData{
		AgentID:         agentID,
		Registered:      false,
		MessagesCleared: true,
	})
}

// handleBatchRegister 处理 E12 POST /api/v1/agents/batch-register（Bearer，幂等）。
func (s *Server) handleBatchRegister(w http.ResponseWriter, r *http.Request) {
	var request BatchRegisterRequest
	if err := decodeJSON(r, &request); err != nil {
		writeFail(w, protocol.CodeBadRequest)
		return
	}
	if len(request.AgentIDs) == 0 {
		writeFail(w, protocol.CodeBadRequest)
		return
	}
	// 去重且保持输入顺序，避免同一 agent 在单事务内被重复处理。
	seen := make(map[string]struct{}, len(request.AgentIDs))
	uniqueIDs := make([]string, 0, len(request.AgentIDs))
	for _, agentID := range request.AgentIDs {
		if agentID == "" {
			continue
		}
		if _, exists := seen[agentID]; exists {
			continue
		}
		seen[agentID] = struct{}{}
		uniqueIDs = append(uniqueIDs, agentID)
	}
	if len(uniqueIDs) == 0 {
		writeFail(w, protocol.CodeBadRequest)
		return
	}

	registered, skipped, deviceIDs, err := s.agentRepo.RegisterBatch(r.Context(), uniqueIDs)
	if err != nil {
		slog.Error("批量注册失败", "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	for index, agentID := range registered {
		if index < len(deviceIDs) {
			s.hub.SendToDevice(deviceIDs[index], protocol.FrameAgentRegistered,
				protocol.AgentRegisteredPayload{AgentID: agentID})
		}
		s.broadcastAgentStatus(r.Context(), agentID, protocol.ReasonRegistered)
	}
	writeOK(w, BatchRegisterData{Registered: registered, Skipped: skipped})
}

// handleUpdateAgentRemark 处理 E13 PATCH /api/v1/agents/{agentId}/remark（Bearer）。
func (s *Server) handleUpdateAgentRemark(w http.ResponseWriter, r *http.Request) {
	var request RemarkRequest
	if err := decodeJSON(r, &request); err != nil {
		writeFail(w, protocol.CodeBadRequest)
		return
	}
	if !validateRemark(request.Remark) {
		writeFail(w, protocol.CodeBadRequest)
		return
	}
	agent, err := s.agentRepo.UpdateRemark(r.Context(), r.PathValue("agentId"), request.Remark)
	if errors.Is(err, ErrAgentNotFound) {
		writeFail(w, protocol.CodeAgentNotFound)
		return
	}
	if err != nil {
		slog.Error("更新 agent 备注失败", "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	writeOK(w, ToAgentDTO(agent))
}

// broadcastAgentStatus 读取单个 agent 的实时三态并广播 F11。
func (s *Server) broadcastAgentStatus(ctx context.Context, agentID string, reason string) {
	agent, err := s.agentRepo.GetByID(ctx, agentID)
	if err != nil {
		slog.Warn("广播 agent.status 前读取状态失败", "agent_id", agentID, "err", err)
		return
	}
	s.hub.BroadcastClients(protocol.FrameAgentStatus, protocol.AgentStatusPayload{
		Agents: []protocol.AgentStatusItem{{
			AgentID:    agent.ID,
			Online:     agent.Online,
			Registered: agent.Registered,
		}},
		Reason: reason,
	})
}
