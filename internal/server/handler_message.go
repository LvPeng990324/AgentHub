package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// handleMessages 处理 E14 GET /api/v1/agents/{agentId}/messages（Bearer）。
//
// 4001 使用约定：已注册但从未发过消息（无会话）时返回 200 + items:[]，不报错；
// 断线重连以 since_seq 补齐，seq 为会话内单调、平台分配的权威序号（L2）。
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agentID := r.PathValue("agentId")
	agent, err := s.agentRepo.GetByID(ctx, agentID)
	if errors.Is(err, ErrAgentNotFound) {
		writeFail(w, protocol.CodeAgentNotFound)
		return
	}
	if err != nil {
		slog.Error("读取历史消息所属 agent 失败", "agent_id", agentID, "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	if !agent.Registered {
		writeFail(w, protocol.CodeAgentNotRegistered)
		return
	}

	session, err := s.messageRepo.GetSessionByAgent(ctx, agentID)
	if errors.Is(err, ErrSessionNotFound) {
		writeOK(w, MessageListData{Items: []MsgDTO{}, HasMore: false, LatestSeq: 0})
		return
	}
	if err != nil {
		slog.Error("读取会话失败", "agent_id", agentID, "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}

	query := ParseListQuery(r)
	messages, hasMore, latestSeq, err := s.messageRepo.ListSinceSeq(ctx, session.ID, query.SinceSeq, query.Limit)
	if err != nil {
		slog.Error("查询历史消息失败", "agent_id", agentID, "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	items := make([]MsgDTO, 0, len(messages))
	for _, message := range messages {
		items = append(items, ToMsgDTO(message))
	}
	writeOK(w, MessageListData{Items: items, HasMore: hasMore, LatestSeq: latestSeq})
}

// handleSession 处理 E15 GET /api/v1/agents/{agentId}/session（Bearer）。
// 无会话时返回 {"session_id":null,"resume_id":"","message_count":0}。
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agentID := r.PathValue("agentId")
	agent, err := s.agentRepo.GetByID(ctx, agentID)
	if errors.Is(err, ErrAgentNotFound) {
		writeFail(w, protocol.CodeAgentNotFound)
		return
	}
	if err != nil {
		slog.Error("读取会话所属 agent 失败", "agent_id", agentID, "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	if !agent.Registered {
		writeFail(w, protocol.CodeAgentNotRegistered)
		return
	}

	session, err := s.messageRepo.GetSessionByAgent(ctx, agentID)
	if errors.Is(err, ErrSessionNotFound) {
		writeOK(w, SessionData{SessionID: nil, ResumeID: "", MessageCount: 0})
		return
	}
	if err != nil {
		slog.Error("读取会话失败", "agent_id", agentID, "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	count, err := s.messageRepo.CountMessages(ctx, session.ID)
	if err != nil {
		slog.Error("统计会话消息数失败", "session_id", session.ID, "err", err)
		writeFail(w, protocol.CodeInternalError)
		return
	}
	sessionID := session.ID
	writeOK(w, SessionData{SessionID: &sessionID, ResumeID: session.ResumeID, MessageCount: count})
}
