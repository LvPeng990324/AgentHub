package adapter

// SessionManager（§8.9 / K9）：sync.Mutex 保护 active map[agentID]CancelFunc，
// 同一 agent 同时仅 1 个活跃 turn；chat.stop / agent.unregistered / 进程退出都走 cancel。
// 适配器不落库、不持久化 resume_id（§8.7）。

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agenthub/agenthub/pkg/protocol"
)

const maxChunkRunes = 1024 // 大段文本按 ≤4KiB/帧 切片（§8.9）

// turnState 是单个活跃 turn 的状态。
// suppressed 由 F12 agent.unregistered 置位：该 turn 收尾时不得再回 chat.done
// （平台已单事务清库，并由 interrupted 兜底前端气泡）。
type turnState struct {
	cancel     context.CancelFunc
	suppressed atomic.Bool
}

// SessionManager 编排单 agent 的一轮对话。
type SessionManager struct {
	rootCtx    context.Context
	backend    Backend
	sender     FrameSender
	discoverer *Discoverer
	registry   *Registry

	// turnTimeout 是单轮硬超时；0 表示禁用（来自 adapter.turn_timeout_s）。
	turnTimeout time.Duration
	// defaultCwd 是 CLI 的工作目录（来自 adapter.working_dir），必须非空：
	// kimi 等真实 ACP CLI 会校验 session/new 的 workspace root，空 cwd 会被拒绝。
	defaultCwd string

	mu     sync.Mutex
	active map[string]*turnState
}

// NewSessionManager 构造对话编排器。
func NewSessionManager(
	rootCtx context.Context,
	backend Backend,
	sender FrameSender,
	discoverer *Discoverer,
	registry *Registry,
	turnTimeout time.Duration,
	defaultCwd string,
) *SessionManager {
	return &SessionManager{
		rootCtx:     rootCtx,
		backend:     backend,
		sender:      sender,
		discoverer:  discoverer,
		registry:    registry,
		turnTimeout: turnTimeout,
		defaultCwd:  defaultCwd,
		active:      make(map[string]*turnState),
	}
}

// OnChatSend 处理平台补齐后的完整 F05 chat.send。
// 消费 session_id / resume_id / user_seq / assistant_seq；**不分配任何 seq**（L2 由平台唯一权威分配）。
func (m *SessionManager) OnChatSend(payload protocol.ChatSendPayload) {
	agentID := payload.AgentID
	agentType := agentTypeFromID(agentID)
	if agentType == "" {
		m.sendError(agentID, payload.SessionID, payload.AssistantSeq, protocol.CodeBadRequest)
		return
	}
	if _, ok := m.registry.Get(agentType); !ok {
		m.sendError(agentID, payload.SessionID, payload.AssistantSeq, protocol.CodeAgentNotFound)
		return
	}
	execPath, ok := m.discoverer.ExecPathFor(agentType)
	if !ok {
		m.sendError(agentID, payload.SessionID, payload.AssistantSeq, protocol.CodeAgentOffline)
		return
	}

	// turn_timeout_s > 0 时给单轮加硬超时，防止 CLI 挂死后该 agent 长期不可用（K8 / 5005）。
	// 超时触发的 cancel 会让 exec.CommandContext 回收进程树，解析器据 ctx.Err() 收尾。
	var turnCtx context.Context
	var cancel context.CancelFunc
	if m.turnTimeout > 0 {
		turnCtx, cancel = context.WithTimeout(m.rootCtx, m.turnTimeout)
	} else {
		turnCtx, cancel = context.WithCancel(m.rootCtx)
	}
	state := m.tryBegin(agentID, cancel)
	if state == nil {
		cancel()
		// U1：不排队、不插话、不自动取消上一轮。
		m.sendError(agentID, payload.SessionID, payload.AssistantSeq, protocol.CodeGenerationInProgress)
		return
	}

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.active, agentID)
			m.mu.Unlock()
			cancel()
		}()

		chunkIndex := 0
		result := m.runTurn(turnCtx, agentID, agentType, execPath, payload.ResumeID, payload, state, &chunkIndex)

		// ★会话恢复被拒 → 降级为新会话（§8.6 / K12 / U4 / R-18）。
		// 集中在此一处覆盖 S 族 4 种 + J 族 2 种（ACP/AS 在各自解析器内已处理），
		// 解析器保持纯粹。只在第一轮「零输出」时重跑：已产出文本再重跑会重复输出。
		if result.Status == ResultFailed && payload.ResumeID != "" && chunkIndex == 0 {
			descriptor, _ := m.registry.Get(agentType)
			if ResumeRejectionUndetectable(agentType) ||
				ClassifyResumeFailure(errors.New(result.Error), descriptor) {
				slog.Warn("会话恢复被拒，降级为新会话",
					"agent_id", agentID, "type", agentType, "reason", result.Error)
				result = m.runTurn(turnCtx, agentID, agentType, execPath, "", payload, state, &chunkIndex)
				result.ResumeRejected = true
			}
		}

		// F12 取消注册后不回任何 chat.done / chat.error，交给平台 interrupted 兜底。
		if state.suppressed.Load() {
			return
		}

		// 失败时把 CLI 的原始报错落到适配器日志（K7：只记错误文本，不记 prompt），
		// 否则前端只看到固定的「Agent 启动失败」，排查时无从下手。
		if result.Status == ResultFailed {
			slog.Error("agent turn 失败",
				"agent_id", agentID, "type", agentType, "err", result.Error)
		}

		resumeID := result.SessionID
		if result.Status == ResultFailed {
			// 「新会话在 prompt 前失败不得上报 session id」：失败一律回带空值。
			resumeID = ""
		}
		// 降级重跑成功时只发 chat.done{completed, resume_rejected:true}；
		// 重跑仍失败才发 chat.error + chat.done{failed}。
		if result.Status == ResultTimeout {
			m.sendError(agentID, payload.SessionID, payload.AssistantSeq, protocol.CodeGenerationTimeout)
		} else if result.Status == ResultFailed {
			m.sendError(agentID, payload.SessionID, payload.AssistantSeq, protocol.CodeCLIStartFailed)
		}

		m.sender.Send(protocol.FrameChatDone, protocol.ChatDonePayload{
			AgentID:        agentID,
			SessionID:      payload.SessionID,
			AssistantSeq:   payload.AssistantSeq,
			Status:         mapResultStatus(result.Status),
			ResumeID:       resumeID,
			ResumeRejected: result.ResumeRejected,
			DurationMS:     result.DurationMs,
			// 前端在失败气泡里折叠展示该文本（>300 字符折叠），这里只做长度兜底。
			Error: truncateErrorDetail(result.Error),
		})
	}()
}

// runTurn 执行一轮 CLI turn：启动 → 消费流式消息（发 chat.chunk）→ 返回终态 Result。
// 只负责「执行 + 发 chunk」，chat.error / chat.done 由调用方统一发：
// 这样降级重跑时不会先向前端发一条错误、成功后又发 completed（自相矛盾）。
// chunkIndex 用指针跨轮延续，重跑时不得重置计数（防 chunk_index 重复）。
func (m *SessionManager) runTurn(
	ctx context.Context,
	agentID string,
	agentType string,
	execPath string,
	resumeID string,
	payload protocol.ChatSendPayload,
	state *turnState,
	chunkIndex *int,
) Result {
	session, err := m.backend.Execute(ctx, payload.Content, ExecOptions{
		AgentType: agentType,
		ExecPath:  execPath,
		// ★ 核心修复：cwd 必须真实存在且非空。kimi 的 ACP session/new 会校验
		// workspace root，空串会被直接拒绝（Internal error: workspace root does not exist）。
		Cwd:             m.defaultCwd,
		ResumeSessionID: resumeID,
	})
	if err != nil {
		return Result{Status: ResultFailed, Error: err.Error()}
	}

	earlySessionID := ""
	errorText := ""
	for message := range session.Messages {
		if state.suppressed.Load() {
			// 取消注册后只排空通道，不再下发任何帧（避免复活已清空的会话）。
			continue
		}
		switch message.Type {
		case MsgText:
			for _, piece := range splitText(message.Content, maxChunkRunes) {
				m.sender.Send(protocol.FrameChatChunk, protocol.ChatChunkPayload{
					AgentID:      agentID,
					SessionID:    payload.SessionID,
					AssistantSeq: payload.AssistantSeq,
					ChunkIndex:   *chunkIndex,
					Delta:        piece,
				})
				*chunkIndex = *chunkIndex + 1
			}
		case MsgError:
			// 记下来，统一由外层决定发 chat.error 还是走降级重跑。
			errorText = message.Content
		case MsgStatus:
			if message.SessionID != "" {
				earlySessionID = message.SessionID
			}
		}
	}

	result, ok := <-session.Result
	if !ok {
		result = Result{Status: ResultFailed, Error: "结果通道意外关闭"}
	}
	if result.Error == "" && errorText != "" {
		result.Error = errorText
	}
	if result.SessionID == "" {
		result.SessionID = earlySessionID
	}
	return result
}

// OnChatStop 处理 F10：cancel 当前 turn → KillTree（由解析器收尾）→ chat.done{stopped} 保留已产出内容。
func (m *SessionManager) OnChatStop(payload protocol.ChatStopPayload) {
	m.mu.Lock()
	state := m.active[payload.AgentID]
	m.mu.Unlock()
	// 停止生成不置 suppressed：仍需回 chat.done{stopped} 并保留已产出内容（R-14）。
	if state != nil {
		state.cancel()
	}
}

// OnUnregistered 处理 F12：只 cancel + 清内存 + 杀子进程，绝不删数据（平台已单事务清空）。
func (m *SessionManager) OnUnregistered(agentID string) {
	m.mu.Lock()
	state := m.active[agentID]
	if state != nil {
		state.suppressed.Store(true)
		state.cancel()
		delete(m.active, agentID)
	}
	m.mu.Unlock()
	slog.Info("已清理 agent 内存态与子进程", "agent_id", agentID)
}

// CancelAll 在适配器退出时取消全部活跃 turn。
func (m *SessionManager) CancelAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for agentID, state := range m.active {
		state.cancel()
		delete(m.active, agentID)
	}
}

// tryBegin 原子地占用单活跃 turn 槽位；返回该 turn 的状态（失败返回 nil）。
func (m *SessionManager) tryBegin(agentID string, cancel context.CancelFunc) *turnState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active[agentID] != nil {
		return nil
	}
	state := &turnState{cancel: cancel}
	m.active[agentID] = state
	return state
}

// ActiveCount 返回当前活跃 turn 数（单测与诊断用）。
func (m *SessionManager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

func (m *SessionManager) sendError(agentID string, sessionID string, assistantSeq int64, code int) {
	m.sender.Send(protocol.FrameChatError, protocol.ChatErrorPayload{
		AgentID:      agentID,
		SessionID:    sessionID,
		AssistantSeq: assistantSeq,
		Code:         code,
		Message:      protocol.ErrorMessage(code),
	})
}

// agentTypeFromID 从 "<device_id>:<type>" 尾段取 type（用最后一个冒号，兼容未来 id 变化）。
func agentTypeFromID(agentID string) string {
	index := strings.LastIndex(agentID, ":")
	if index < 0 || index == len(agentID)-1 {
		return ""
	}
	return agentID[index+1:]
}

// mapResultStatus 把内部 Result.Status 映射为 chat.done 的终态拼写（§8.9）。
func mapResultStatus(status string) string {
	switch status {
	case ResultCompleted:
		return protocol.DoneStatusCompleted
	case ResultCancelled:
		return protocol.DoneStatusStopped
	case ResultTimeout, ResultAborted:
		return protocol.DoneStatusInterrupted
	case ResultFailed:
		return protocol.DoneStatusFailed
	default:
		return protocol.DoneStatusFailed
	}
}

// splitText 按 rune 把大段文本切成 ≤maxRunes 的片段，避免切断 UTF-8 与超大帧。
func splitText(text string, maxRunes int) []string {
	if text == "" {
		return nil
	}
	if maxRunes <= 0 {
		maxRunes = maxChunkRunes
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return []string{text}
	}
	parts := make([]string, 0, len(runes)/maxRunes+1)
	for start := 0; start < len(runes); start += maxRunes {
		end := start + maxRunes
		if end > len(runes) {
			end = len(runes)
		}
		parts = append(parts, string(runes[start:end]))
	}
	return parts
}

// turnTimeout 是解析器握手/生成的默认预算（平台侧另有兜底）。
const defaultRPCTimeout = 10 * time.Minute

// maxErrorDetailRunes 限制 chat.done.error 的长度：CLI 异常输出可能非常长，
// 直接塞进 WS 帧会浪费带宽；前端另有 300 字符折叠展示。
const maxErrorDetailRunes = 2000

// truncateErrorDetail 按 rune 截断错误详情（不切断 UTF-8）。
func truncateErrorDetail(text string) string {
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxErrorDetailRunes {
		return text
	}
	return string(runes[:maxErrorDetailRunes]) + "…"
}
