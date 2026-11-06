package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// sqlRunner 抽象 *sql.DB 与 *sql.Tx，使「注册 + 建会话」能在单事务内复用同一实现，
// 避免跨 Repo 出现重复 SQL（K10 / 全局一致性）。
type sqlRunner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ensureSessionWith 保证一个 agent 有且仅有一条会话（agent_id UNIQUE 为库级保证）。
// INSERT ... ON CONFLICT(agent_id) DO NOTHING 保证幂等，随后回读实际会话行。
func ensureSessionWith(ctx context.Context, runner sqlRunner, agentID string) (Session, error) {
	now := nowUTC()
	sessionID := "ses_" + uuid.NewString()
	if _, err := runner.ExecContext(ctx, `
INSERT INTO sessions(id, agent_id, resume_id, created_at, updated_at)
VALUES(?, ?, '', ?, ?)
ON CONFLICT(agent_id) DO NOTHING`, sessionID, agentID, now, now); err != nil {
		return Session{}, fmt.Errorf("创建会话失败: %w", err)
	}
	var session Session
	if err := runner.QueryRowContext(ctx,
		`SELECT id, agent_id, resume_id, created_at, updated_at FROM sessions WHERE agent_id = ?`,
		agentID).Scan(&session.ID, &session.AgentID, &session.ResumeID, &session.CreatedAt, &session.UpdatedAt); err != nil {
		return Session{}, fmt.Errorf("回读会话失败: %w", err)
	}
	return session, nil
}

// StaleStream 描述一条残留的 streaming 占位消息，供 InterruptStaleStreaming 兜底。
type StaleStream struct {
	SessionID string
	Seq       int64
	AgentID   string
	DeviceID  string
}

// MessageRepo 封装 sessions / messages 表读写。
type MessageRepo struct {
	store *Store
}

// NewMessageRepo 构造消息 Repo。
func NewMessageRepo(store *Store) *MessageRepo {
	return &MessageRepo{store: store}
}

// EnsureSession 幂等创建/回读某 agent 的单会话。
func (r *MessageRepo) EnsureSession(ctx context.Context, agentID string) (Session, error) {
	return ensureSessionWith(ctx, r.store.DB, agentID)
}

// GetSessionByAgent 读取会话；无会话返回 ErrSessionNotFound。
func (r *MessageRepo) GetSessionByAgent(ctx context.Context, agentID string) (Session, error) {
	var session Session
	err := r.store.DB.QueryRowContext(ctx,
		`SELECT id, agent_id, resume_id, created_at, updated_at FROM sessions WHERE agent_id = ?`,
		agentID).Scan(&session.ID, &session.AgentID, &session.ResumeID, &session.CreatedAt, &session.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("读取会话失败: %w", err)
	}
	return session, nil
}

// GetResumeID 返回平台侧持久化的会话恢复标识；无会话时返回空串（首轮）。
func (r *MessageRepo) GetResumeID(ctx context.Context, agentID string) (string, error) {
	var resumeID string
	err := r.store.DB.QueryRowContext(ctx,
		`SELECT resume_id FROM sessions WHERE agent_id = ?`, agentID).Scan(&resumeID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取会话恢复标识失败: %w", err)
	}
	return resumeID, nil
}

// UpdateResumeID 在 chat.done 时写回新的恢复标识（空串表示不更新）。
func (r *MessageRepo) UpdateResumeID(ctx context.Context, sessionID string, resumeID string) error {
	if resumeID == "" {
		return nil
	}
	_, err := r.store.DB.ExecContext(ctx,
		`UPDATE sessions SET resume_id = ?, updated_at = ? WHERE id = ?`,
		resumeID, nowUTC(), sessionID)
	if err != nil {
		return fmt.Errorf("更新会话恢复标识失败: %w", err)
	}
	return nil
}

// AppendMessage 追加单条消息（system 消息或测试造数使用）。
func (r *MessageRepo) AppendMessage(ctx context.Context, sessionID string, seq int64, role string, content string, status string) error {
	if _, err := r.store.DB.ExecContext(ctx, `
INSERT INTO messages(session_id, seq, role, content, status, created_at)
VALUES(?, ?, ?, ?, ?, ?)`, sessionID, seq, role, content, status, nowUTC()); err != nil {
		return fmt.Errorf("追加消息失败: %w", err)
	}
	return nil
}

// AppendTurn 是 chat.send 的落库入口：**单事务内**分配会话级 seq（L2）并写入两条消息：
//   - user（status=complete，完整 prompt）；
//   - assistant 占位（content=""，status=streaming）。
//
// seq = COALESCE(MAX(seq),0)+1 / +2，落库、不随 WS 重连重置；UNIQUE(session_id, seq) 兜底。
// 流式 chunk 不逐条落库，chat.done 时再一次性 UPDATE 完整文本（§7.4 避免写放大）。
func (r *MessageRepo) AppendTurn(ctx context.Context, sessionID string, userContent string) (int64, int64, error) {
	tx, err := r.store.BeginTx(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("开启消息事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var maxSeq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM messages WHERE session_id = ?`, sessionID).Scan(&maxSeq); err != nil {
		return 0, 0, fmt.Errorf("读取会话最大 seq 失败: %w", err)
	}
	userSeq := maxSeq + 1
	assistantSeq := maxSeq + 2
	now := nowUTC()

	if _, err := tx.ExecContext(ctx, `
INSERT INTO messages(session_id, seq, role, content, status, created_at)
VALUES(?, ?, ?, ?, ?, ?)`,
		sessionID, userSeq, protocol.RoleUser, userContent, protocol.MessageStatusComplete, now); err != nil {
		return 0, 0, fmt.Errorf("写入用户消息失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO messages(session_id, seq, role, content, status, created_at)
VALUES(?, ?, ?, ?, ?, ?)`,
		sessionID, assistantSeq, protocol.RoleAssistant, "", protocol.MessageStatusStreaming, now); err != nil {
		return 0, 0, fmt.Errorf("写入助手占位消息失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("提交消息事务失败: %w", err)
	}
	return userSeq, assistantSeq, nil
}

// UpdateMessageContent 在 chat.done 时一次性落库完整文本与终态。
func (r *MessageRepo) UpdateMessageContent(ctx context.Context, sessionID string, seq int64, content string, status string) error {
	result, err := r.store.DB.ExecContext(ctx,
		`UPDATE messages SET content = ?, status = ? WHERE session_id = ? AND seq = ?`,
		content, status, sessionID, seq)
	if err != nil {
		return fmt.Errorf("落库助手消息失败: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取助手消息更新结果失败: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("未找到会话 %s 的 seq=%d 占位消息", sessionID, seq)
	}
	return nil
}

// UpdateStreamingStatus 把残留占位消息从 streaming 改为终态（error / stopped / interrupted）。
func (r *MessageRepo) UpdateStreamingStatus(ctx context.Context, sessionID string, seq int64, status string) error {
	_, err := r.store.DB.ExecContext(ctx,
		`UPDATE messages SET status = ? WHERE session_id = ? AND seq = ? AND status = ?`,
		status, sessionID, seq, protocol.MessageStatusStreaming)
	if err != nil {
		return fmt.Errorf("更新流式占位状态失败: %w", err)
	}
	return nil
}

// ListSinceSeq 按会话内 seq 单调递增返回历史消息，供 since_seq 补齐（R-16）。
// 返回 items / has_more / latest_seq；latest_seq 为当前会话最大 seq（含本次未返回的部分）。
func (r *MessageRepo) ListSinceSeq(ctx context.Context, sessionID string, sinceSeq int64, limit int) ([]Msg, bool, int64, error) {
	var latestSeq int64
	if err := r.store.DB.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM messages WHERE session_id = ?`, sessionID).Scan(&latestSeq); err != nil {
		return nil, false, 0, fmt.Errorf("读取最新 seq 失败: %w", err)
	}
	rows, err := r.store.DB.QueryContext(ctx, `
SELECT id, session_id, seq, role, content, status, created_at
FROM messages WHERE session_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`,
		sessionID, sinceSeq, limit+1)
	if err != nil {
		return nil, false, 0, fmt.Errorf("查询历史消息失败: %w", err)
	}
	defer rows.Close()

	messages := make([]Msg, 0)
	for rows.Next() {
		var message Msg
		if err := rows.Scan(&message.ID, &message.SessionID, &message.Seq, &message.Role,
			&message.Content, &message.Status, &message.CreatedAt); err != nil {
			return nil, false, 0, fmt.Errorf("扫描历史消息失败: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, false, 0, fmt.Errorf("遍历历史消息失败: %w", err)
	}
	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}
	return messages, hasMore, latestSeq, nil
}

// CountMessages 返回某会话的消息条数（E15 message_count）。
func (r *MessageRepo) CountMessages(ctx context.Context, sessionID string) (int64, error) {
	var count int64
	if err := r.store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&count); err != nil {
		return 0, fmt.Errorf("统计消息数量失败: %w", err)
	}
	return count, nil
}

// PurgeByAgent 清空某 agent 的会话：DELETE FROM sessions WHERE agent_id=?，
// 依赖 foreign_keys=ON 级联删除该会话下全部 messages（E11 第二步 / 验收要点）。
// 绝不删 agents / devices 行，也不清 remark。
func (r *MessageRepo) PurgeByAgent(ctx context.Context, agentID string) (int64, error) {
	result, err := r.store.DB.ExecContext(ctx,
		`DELETE FROM sessions WHERE agent_id = ?`, agentID)
	if err != nil {
		return 0, fmt.Errorf("清空会话失败: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清空会话结果失败: %w", err)
	}
	return affected, nil
}

// ListStaleStreaming 查询残留的 streaming 占位消息；deviceID 为空表示全库扫描（启动时兜底）。
func (r *MessageRepo) ListStaleStreaming(ctx context.Context, deviceID string) ([]StaleStream, error) {
	query := `
SELECT m.session_id, m.seq, s.agent_id, a.device_id
FROM messages m
JOIN sessions s ON s.id = m.session_id
JOIN agents a ON a.id = s.agent_id
WHERE m.status = ?`
	args := []any{protocol.MessageStatusStreaming}
	if deviceID != "" {
		query += ` AND a.device_id = ?`
		args = append(args, deviceID)
	}
	query += ` ORDER BY m.session_id, m.seq`

	rows, err := r.store.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询残留流式消息失败: %w", err)
	}
	defer rows.Close()

	stale := make([]StaleStream, 0)
	for rows.Next() {
		var item StaleStream
		if err := rows.Scan(&item.SessionID, &item.Seq, &item.AgentID, &item.DeviceID); err != nil {
			return nil, fmt.Errorf("扫描残留流式消息失败: %w", err)
		}
		stale = append(stale, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历残留流式消息失败: %w", err)
	}
	return stale, nil
}
