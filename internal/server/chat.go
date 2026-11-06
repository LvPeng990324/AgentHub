package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// streamBuffer 是单条助手消息的内存增量缓冲（§7.4 L3）。
// chunk 不逐条落库；chat.done 到达时按 chunk_index 顺序拼接完整文本，一次性 UPDATE 落库。
type streamBuffer struct {
	AgentID      string
	DeviceID     string
	SessionID    string
	AssistantSeq int64
	Chunks       map[int]string
	MaxIndex     int
}

// assemble 按 chunk_index 从 0 递增拼接，缺失的下标留空（保证去重且顺序稳定）。
func (b *streamBuffer) assemble() string {
	var builder strings.Builder
	for index := 0; index <= b.MaxIndex; index++ {
		builder.WriteString(b.Chunks[index])
	}
	return builder.String()
}

// ChatOrchestrator 编排一次完整对话：chat.send 落库+分配 seq+转发；chunk 内存缓冲；
// chat.done 一次性落库；chat.stop / 取消注册 / 离线时中断兜底。
//
// mu 只保护 streams 与 active 两张内存表，不覆盖任何 DB 操作或 Hub 广播（K9）。
type ChatOrchestrator struct {
	store       *Store
	deviceRepo  *DeviceRepo
	agentRepo   *AgentRepo
	messageRepo *MessageRepo
	hub         *Hub

	mu      sync.Mutex
	streams map[string]*streamBuffer // key = "<session_id>:<assistant_seq>"
	active  map[string]bool          // agent_id → 是否存在活跃 turn（U1 同一 agent 单活跃）
}

// NewChatOrchestrator 构造对话编排器。
func NewChatOrchestrator(
	store *Store,
	deviceRepo *DeviceRepo,
	agentRepo *AgentRepo,
	messageRepo *MessageRepo,
	hub *Hub,
) *ChatOrchestrator {
	return &ChatOrchestrator{
		store:       store,
		deviceRepo:  deviceRepo,
		agentRepo:   agentRepo,
		messageRepo: messageRepo,
		hub:         hub,
		streams:     make(map[string]*streamBuffer),
		active:      make(map[string]bool),
	}
}

// streamKey 生成内存缓冲键（会话 id + 助手消息 seq）。
func streamKey(sessionID string, assistantSeq int64) string {
	return fmt.Sprintf("%s:%d", sessionID, assistantSeq)
}

// OnClientSend 处理 F05 chat.send（浏览器 → 平台 → 适配器）。
//
// 顺序：校验三态与并发 → 取 resume_id → EnsureSession → 单事务分配会话级 seq 并预插
// user + assistant(streaming) 占位 → F06 send_ack → 转发 F05 给设备。
func (c *ChatOrchestrator) OnClientSend(ctx context.Context, conn *WSConn, payload protocol.ChatSendPayload) {
	agentID := payload.AgentID
	if agentID == "" || payload.Content == "" {
		c.sendError(conn, agentID, protocol.CodeBadRequest)
		return
	}
	if len(payload.Content) > protocol.MaxPromptBytes {
		c.sendError(conn, agentID, protocol.CodeMessageTooLarge)
		return
	}
	if utf8.RuneCountInString(payload.Content) == 0 {
		c.sendError(conn, agentID, protocol.CodeBadRequest)
		return
	}

	// ★原子占位必须发生在任何 DB 读写之前：否则并发 chat.send 会全部通过
	// 「先检查后置位」的窗口（TOCTOU），出现多份 send_ack / 多个 streaming 占位。
	// 此后**每一个失败分支都必须 releaseActive**，否则该 agent 会永久返回 4003。
	if !c.tryBegin(agentID) {
		// U1：不排队、不插话、不自动取消上一轮。
		c.sendError(conn, agentID, protocol.CodeGenerationInProgress)
		return
	}

	agent, err := c.agentRepo.GetByID(ctx, agentID)
	if err != nil {
		c.releaseActive(agentID)
		if err == ErrAgentNotFound {
			c.sendError(conn, agentID, protocol.CodeAgentNotFound)
		} else {
			slog.Error("chat.send 读取 agent 失败", "agent_id", agentID, "err", err)
			c.sendError(conn, agentID, protocol.CodeInternalError)
		}
		return
	}
	if !agent.Registered {
		c.releaseActive(agentID)
		c.sendError(conn, agentID, protocol.CodeAgentNotRegistered)
		return
	}
	if !agent.Online || !c.hub.HasDeviceConnection(agent.DeviceID) {
		c.releaseActive(agentID)
		c.sendError(conn, agentID, protocol.CodeAgentOffline)
		return
	}

	resumeID, err := c.messageRepo.GetResumeID(ctx, agentID)
	if err != nil {
		c.releaseActive(agentID)
		slog.Error("chat.send 读取 resume_id 失败", "agent_id", agentID, "err", err)
		c.sendError(conn, agentID, protocol.CodeInternalError)
		return
	}
	session, err := c.messageRepo.EnsureSession(ctx, agentID)
	if err != nil {
		c.releaseActive(agentID)
		slog.Error("chat.send 创建会话失败", "agent_id", agentID, "err", err)
		c.sendError(conn, agentID, protocol.CodeInternalError)
		return
	}

	// ★单事务分配会话级 seq（L2）：user_seq = max+1，assistant_seq = max+2。
	userSeq, assistantSeq, err := c.messageRepo.AppendTurn(ctx, session.ID, payload.Content)
	if err != nil {
		c.releaseActive(agentID)
		slog.Error("chat.send 分配会话 seq 失败", "session_id", session.ID, "err", err)
		c.sendError(conn, agentID, protocol.CodeInternalError)
		return
	}

	buffer := &streamBuffer{
		AgentID:      agentID,
		DeviceID:     agent.DeviceID,
		SessionID:    session.ID,
		AssistantSeq: assistantSeq,
		Chunks:       make(map[int]string),
		MaxIndex:     -1,
	}
	// active 已由 tryBegin 提前置位，此处只登记流式缓冲。
	c.mu.Lock()
	c.streams[streamKey(session.ID, assistantSeq)] = buffer
	c.mu.Unlock()

	createdAt := nowUTC()
	c.hub.SendToConn(conn, protocol.FrameChatSendAck, protocol.ChatSendAckPayload{
		AgentID:      agentID,
		SessionID:    session.ID,
		UserSeq:      userSeq,
		AssistantSeq: assistantSeq,
		CreatedAt:    createdAt,
	})

	forward := protocol.ChatSendPayload{
		AgentID:      agentID,
		Content:      payload.Content,
		SessionID:    session.ID,
		UserSeq:      userSeq,
		AssistantSeq: assistantSeq,
		ResumeID:     resumeID,
	}
	if !c.hub.SendToDevice(agent.DeviceID, protocol.FrameChatSend, forward) {
		// 设备 WS 刚好断开（45s 宽限内 DB 仍 online）：不转发、不留半截气泡。
		slog.Warn("chat.send 转发失败：设备连接不可用", "device_id", agent.DeviceID)
		c.abortTurn(ctx, agentID, session.ID, assistantSeq, protocol.MessageStatusInterrupted)
		c.sendError(conn, agentID, protocol.CodeAgentOffline)
		return
	}

	// 脱敏日志：只打 prompt 长度，不打全文（K7）。
	slog.Info("chat.send 已编排",
		"agent_id", agentID, "session_id", session.ID,
		"user_seq", userSeq, "assistant_seq", assistantSeq,
		"prompt_bytes", len(payload.Content))
}

// OnAdapterChunk 处理 F07：内存缓冲（chunk_index 去重）+ 原样透传给浏览器，不逐条落库。
func (c *ChatOrchestrator) OnAdapterChunk(ctx context.Context, payload protocol.ChatChunkPayload) {
	key := streamKey(payload.SessionID, payload.AssistantSeq)
	c.mu.Lock()
	buffer := c.streams[key]
	if buffer != nil {
		if _, exists := buffer.Chunks[payload.ChunkIndex]; !exists {
			buffer.Chunks[payload.ChunkIndex] = payload.Delta
			if payload.ChunkIndex > buffer.MaxIndex {
				buffer.MaxIndex = payload.ChunkIndex
			}
		} else {
			// 重复 chunk：仍透传（前端按 chunk_index 去重），但不重复拼接。
			slog.Debug("丢弃重复 chunk", "session_id", payload.SessionID,
				"assistant_seq", payload.AssistantSeq, "chunk_index", payload.ChunkIndex)
		}
	}
	c.mu.Unlock()

	// 平台仅重分配连接级 seq（由 Hub.SendToConn 完成），payload 原样透传。
	c.hub.BroadcastClients(protocol.FrameChatChunk, payload)
}

// OnAdapterDone 处理 F08：一次性 UPDATE 完整文本与状态、写回 resume_id、广播终态、释放活跃 turn。
func (c *ChatOrchestrator) OnAdapterDone(ctx context.Context, payload protocol.ChatDonePayload) {
	key := streamKey(payload.SessionID, payload.AssistantSeq)
	buffer := c.takeStream(key)
	if buffer != nil {
		if payload.SessionID == "" {
			payload.SessionID = buffer.SessionID
		}
		if payload.AssistantSeq == 0 {
			payload.AssistantSeq = buffer.AssistantSeq
		}
		if payload.AgentID == "" {
			payload.AgentID = buffer.AgentID
		}
		if err := c.messageRepo.UpdateMessageContent(ctx,
			buffer.SessionID, buffer.AssistantSeq, buffer.assemble(),
			mapDoneStatus(payload.Status)); err != nil {
			slog.Error("chat.done 落库助手消息失败",
				"session_id", buffer.SessionID, "assistant_seq", buffer.AssistantSeq, "err", err)
		}
	}
	if payload.ResumeID != "" {
		if err := c.messageRepo.UpdateResumeID(ctx, payload.SessionID, payload.ResumeID); err != nil {
			slog.Error("chat.done 写回 resume_id 失败", "session_id", payload.SessionID, "err", err)
		}
	}
	if payload.AgentID != "" {
		c.releaseActive(payload.AgentID)
	}
	c.hub.BroadcastClients(protocol.FrameChatDone, payload)
	slog.Info("chat.done 已落库",
		"agent_id", payload.AgentID, "session_id", payload.SessionID,
		"assistant_seq", payload.AssistantSeq, "status", payload.Status)
}

// OnAdapterError 处理 F09：占位消息改 error、广播错误、释放活跃 turn。
func (c *ChatOrchestrator) OnAdapterError(ctx context.Context, payload protocol.ChatErrorPayload) {
	key := streamKey(payload.SessionID, payload.AssistantSeq)
	buffer := c.takeStream(key)
	if buffer != nil {
		if err := c.messageRepo.UpdateStreamingStatus(ctx,
			buffer.SessionID, buffer.AssistantSeq, protocol.MessageStatusError); err != nil {
			slog.Error("chat.error 更新占位状态失败", "session_id", buffer.SessionID, "err", err)
		}
		c.releaseActive(buffer.AgentID)
	}
	if payload.Code == 0 {
		payload.Code = protocol.CodeInternalError
	}
	if payload.Message == "" {
		payload.Message = protocol.ErrorMessage(payload.Code)
	}
	c.hub.BroadcastClients(protocol.FrameChatError, payload)
}

// Stop 处理 F10 chat.stop：把停止请求转发给设备；设备无连接时本地兜底为 stopped。
func (c *ChatOrchestrator) Stop(ctx context.Context, payload protocol.ChatStopPayload) {
	if payload.AgentID == "" {
		return
	}
	deviceID, err := c.agentRepo.DeviceIDOf(ctx, payload.AgentID)
	if err != nil {
		slog.Warn("chat.stop 查询设备失败", "agent_id", payload.AgentID, "err", err)
		return
	}
	if c.hub.SendToDevice(deviceID, protocol.FrameChatStop, payload) {
		return
	}
	// 设备已断：直接把该 agent 全部 streaming 占位收尾，避免永久悬挂。
	stale, err := c.messageRepo.ListStaleStreaming(ctx, deviceID)
	if err != nil {
		slog.Error("chat.stop 兜底查询失败", "device_id", deviceID, "err", err)
		return
	}
	for _, item := range stale {
		if item.AgentID != payload.AgentID {
			continue
		}
		if err := c.messageRepo.UpdateStreamingStatus(ctx,
			item.SessionID, item.Seq, protocol.MessageStatusStopped); err != nil {
			slog.Error("chat.stop 兜底更新失败", "session_id", item.SessionID, "err", err)
		}
		c.takeStream(streamKey(item.SessionID, item.Seq))
		c.BroadcastDone(protocol.ChatDonePayload{
			AgentID:      item.AgentID,
			SessionID:    item.SessionID,
			AssistantSeq: item.Seq,
			Status:       protocol.DoneStatusStopped,
		})
	}
	c.releaseActive(payload.AgentID)
}

// InterruptStaleStreaming 把残留 streaming 占位消息批量兜底为 interrupted，
// 并下行 chat.done{status:interrupted}（§7.5：启动 / 设备离线 / 重连对账时调用）。
// deviceID 为空表示全库扫描（进程启动时）。返回受影响条数。
func (c *ChatOrchestrator) InterruptStaleStreaming(ctx context.Context, deviceID string) (int64, error) {
	stale, err := c.messageRepo.ListStaleStreaming(ctx, deviceID)
	if err != nil {
		return 0, err
	}
	var affected int64
	for _, item := range stale {
		if err := c.messageRepo.UpdateStreamingStatus(ctx,
			item.SessionID, item.Seq, protocol.MessageStatusInterrupted); err != nil {
			return affected, err
		}
		c.takeStream(streamKey(item.SessionID, item.Seq))
		c.releaseActive(item.AgentID)
		c.BroadcastDone(protocol.ChatDonePayload{
			AgentID:      item.AgentID,
			SessionID:    item.SessionID,
			AssistantSeq: item.Seq,
			Status:       protocol.DoneStatusInterrupted,
			Error:        "",
		})
		affected++
	}
	if affected > 0 {
		slog.Info("已兜底残留流式消息", "device_id", deviceID, "count", affected)
	}
	return affected, nil
}

// BroadcastDone 广播 F08 chat.done（供 chat.done / chat.stop / E11 取消注册共用）。
func (c *ChatOrchestrator) BroadcastDone(payload protocol.ChatDonePayload) {
	c.hub.BroadcastClients(protocol.FrameChatDone, payload)
}

// ForgetStreams 丢弃某 agent 的全部内存流式缓冲并释放活跃标记（E11 取消注册后调用，不落库）。
func (c *ChatOrchestrator) ForgetStreams(agentID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, buffer := range c.streams {
		if buffer.AgentID == agentID {
			delete(c.streams, key)
		}
	}
	delete(c.active, agentID)
}

// ---------- 内部辅助 ----------

// sendError 给发起请求的浏览器连接单独下发 F09 chat.error。
func (c *ChatOrchestrator) sendError(conn *WSConn, agentID string, code int) {
	c.hub.SendToConn(conn, protocol.FrameChatError, protocol.ChatErrorPayload{
		AgentID: agentID,
		Code:    code,
		Message: protocol.ErrorMessage(code),
	})
}

// takeStream 取出并删除一个流式缓冲。
func (c *ChatOrchestrator) takeStream(key string) *streamBuffer {
	c.mu.Lock()
	defer c.mu.Unlock()
	buffer := c.streams[key]
	if buffer != nil {
		delete(c.streams, key)
	}
	return buffer
}

// abortTurn 在转发失败时收尾：占位改终态、清缓冲、释放活跃标记。
func (c *ChatOrchestrator) abortTurn(ctx context.Context, agentID string, sessionID string, assistantSeq int64, status string) {
	if err := c.messageRepo.UpdateStreamingStatus(ctx, sessionID, assistantSeq, status); err != nil {
		slog.Error("收尾助手占位失败", "session_id", sessionID, "err", err)
	}
	c.takeStream(streamKey(sessionID, assistantSeq))
	c.releaseActive(agentID)
}

// tryBegin 原子占用单活跃 turn 槽位；已被占用返回 false（U1 同一 agent 单活跃）。
// 与 internal/adapter/session.go 的 tryBegin 同语义：检查与置位在同一把锁内完成，
// 消除「先 isActive 检查、再做多次 DB 往返、最后置位」的 TOCTOU 窗口。
func (c *ChatOrchestrator) tryBegin(agentID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active[agentID] {
		return false
	}
	c.active[agentID] = true
	return true
}

// releaseActive 释放单活跃 turn 槽位（只清 active，不动 streams）。
// ★tryBegin 之后的每个失败分支都必须调用它，否则该 agent 会永久返回 4003。
func (c *ChatOrchestrator) releaseActive(agentID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active, agentID)
}

// mapDoneStatus 把 chat.done 的终态映射到 messages.status（§7.3 两者拼写不同，禁止混用）。
func mapDoneStatus(doneStatus string) string {
	switch doneStatus {
	case protocol.DoneStatusCompleted:
		return protocol.MessageStatusComplete
	case protocol.DoneStatusStopped:
		return protocol.MessageStatusStopped
	case protocol.DoneStatusInterrupted:
		return protocol.MessageStatusInterrupted
	case protocol.DoneStatusFailed:
		return protocol.MessageStatusError
	default:
		return protocol.MessageStatusComplete
	}
}
