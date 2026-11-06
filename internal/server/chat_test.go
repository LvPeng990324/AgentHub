package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/agenthub/agenthub/internal/config"
	"github.com/agenthub/agenthub/pkg/protocol"
)

// newTestChat 构造一套带真实 Repo 的编排器（Hub 无真实 WS 连接，广播为空操作）。
func newTestChat(t *testing.T, store *Store) *ChatOrchestrator {
	t.Helper()
	deviceRepo := NewDeviceRepo(store)
	agentRepo := NewAgentRepo(store)
	messageRepo := NewMessageRepo(store)
	hub := NewHub(&config.ServerConfig{}, store, deviceRepo, agentRepo, messageRepo)
	chat := NewChatOrchestrator(store, deviceRepo, agentRepo, messageRepo, hub)
	hub.AttachChat(chat)
	return chat
}

// TestAppendTurnSeqMonotonic 校验会话级 seq（L2）在多轮对话中严格单调递增，
// 且每轮都预插一条 assistant streaming 占位（不随 WS 重连重置）。
func TestAppendTurnSeqMonotonic(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	insertFixture(t, store, "dev_seq", "dev_seq:claude", protocol.AgentTypeClaude, 0)
	repo := NewMessageRepo(store)
	session, err := repo.GetSessionByAgent(ctx, "dev_seq:claude")
	if err != nil {
		t.Fatalf("读取会话失败: %v", err)
	}

	for turn := 1; turn <= 3; turn++ {
		userSeq, assistantSeq, err := repo.AppendTurn(ctx, session.ID, fmt.Sprintf("第 %d 轮问题", turn))
		if err != nil {
			t.Fatalf("第 %d 轮 AppendTurn 失败: %v", turn, err)
		}
		wantUser := int64(2*turn - 1)
		wantAssistant := int64(2 * turn)
		if userSeq != wantUser || assistantSeq != wantAssistant {
			t.Errorf("第 %d 轮 seq 期望 (%d,%d)，实际 (%d,%d)",
				turn, wantUser, wantAssistant, userSeq, assistantSeq)
		}
	}

	// 落库顺序与状态：6 条消息，seq 1..6，偶数位为 assistant/streaming 占位。
	rows, err := store.DB.QueryContext(ctx,
		`SELECT seq, role, status FROM messages WHERE session_id = ? ORDER BY seq`, session.ID)
	if err != nil {
		t.Fatalf("查询消息失败: %v", err)
	}
	defer rows.Close()
	var seq int64
	for rows.Next() {
		var gotSeq int64
		var role, status string
		if err := rows.Scan(&gotSeq, &role, &status); err != nil {
			t.Fatalf("扫描消息失败: %v", err)
		}
		seq++
		if gotSeq != seq {
			t.Errorf("seq 应连续单调，期望 %d，实际 %d", seq, gotSeq)
		}
		wantRole := protocol.RoleUser
		wantStatus := protocol.MessageStatusComplete
		if seq%2 == 0 {
			wantRole = protocol.RoleAssistant
			wantStatus = protocol.MessageStatusStreaming
		}
		if role != wantRole || status != wantStatus {
			t.Errorf("seq=%d 期望 %s/%s，实际 %s/%s", seq, wantRole, wantStatus, role, status)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历消息失败: %v", err)
	}
	if seq != 6 {
		t.Errorf("应有 6 条消息，实际 %d", seq)
	}

	// chat.done 一次性落库完整文本。
	if err := repo.UpdateMessageContent(ctx, session.ID, 2, "完整回复", protocol.MessageStatusComplete); err != nil {
		t.Fatalf("落库助手消息失败: %v", err)
	}
	messages, _, latestSeq, err := repo.ListSinceSeq(ctx, session.ID, 0, 100)
	if err != nil {
		t.Fatalf("ListSinceSeq 失败: %v", err)
	}
	if latestSeq != 6 || len(messages) != 6 {
		t.Errorf("latest_seq 期望 6、条数 6，实际 %d / %d", latestSeq, len(messages))
	}
	if messages[1].Content != "完整回复" {
		t.Errorf("chat.done 后完整文本应落库，实际 %q", messages[1].Content)
	}
	// since_seq 补齐不重复不丢序。
	partial, _, _, err := repo.ListSinceSeq(ctx, session.ID, 4, 100)
	if err != nil {
		t.Fatalf("ListSinceSeq(since_seq=4) 失败: %v", err)
	}
	if len(partial) != 2 || partial[0].Seq != 5 || partial[1].Seq != 6 {
		t.Errorf("since_seq 补齐错误: %+v", partial)
	}
}

// TestChunkBufferDedupAndAssemble 校验 chunk_index 去重拼接（L3）与「chunk 不逐条落库」。
func TestChunkBufferDedupAndAssemble(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	insertFixture(t, store, "dev_chunk", "dev_chunk:claude", protocol.AgentTypeClaude, 0)
	repo := NewMessageRepo(store)
	session, err := repo.GetSessionByAgent(ctx, "dev_chunk:claude")
	if err != nil {
		t.Fatalf("读取会话失败: %v", err)
	}
	_, assistantSeq, err := repo.AppendTurn(ctx, session.ID, "问题")
	if err != nil {
		t.Fatalf("AppendTurn 失败: %v", err)
	}

	chat := newTestChat(t, store)
	chat.mu.Lock()
	chat.streams[streamKey(session.ID, assistantSeq)] = &streamBuffer{
		AgentID: "dev_chunk:claude", DeviceID: "dev_chunk", SessionID: session.ID,
		AssistantSeq: assistantSeq, Chunks: make(map[int]string), MaxIndex: -1,
	}
	chat.mu.Unlock()

	chunks := []protocol.ChatChunkPayload{
		{SessionID: session.ID, AssistantSeq: assistantSeq, ChunkIndex: 0, Delta: "我先"},
		{SessionID: session.ID, AssistantSeq: assistantSeq, ChunkIndex: 0, Delta: "重复应丢弃"},
		{SessionID: session.ID, AssistantSeq: assistantSeq, ChunkIndex: 2, Delta: "……"},
		{SessionID: session.ID, AssistantSeq: assistantSeq, ChunkIndex: 1, Delta: "检查一下"},
	}
	for _, chunk := range chunks {
		chat.OnAdapterChunk(ctx, chunk)
	}
	key := streamKey(session.ID, assistantSeq)
	chat.mu.Lock()
	buffer := chat.streams[key]
	chat.mu.Unlock()
	if buffer == nil {
		t.Fatal("流式缓冲丢失")
	}
	if got := buffer.assemble(); got != "我先检查一下……" {
		t.Errorf("拼接结果错误: %q", got)
	}
	// chunk 阶段消息内容仍为空、状态仍为 streaming（不逐条落库）。
	var content, status string
	if err := store.DB.QueryRowContext(ctx,
		`SELECT content, status FROM messages WHERE session_id = ? AND seq = ?`,
		session.ID, assistantSeq).Scan(&content, &status); err != nil {
		t.Fatalf("读取占位消息失败: %v", err)
	}
	if content != "" || status != protocol.MessageStatusStreaming {
		t.Errorf("chunk 不得落库，实际 content=%q status=%s", content, status)
	}
}

// TestInterruptStaleStreaming 校验启动/离线时把残留 streaming 占位兜底为 interrupted。
func TestInterruptStaleStreaming(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	insertFixture(t, store, "dev_stale", "dev_stale:claude", protocol.AgentTypeClaude, 0)
	repo := NewMessageRepo(store)
	session, err := repo.GetSessionByAgent(ctx, "dev_stale:claude")
	if err != nil {
		t.Fatalf("读取会话失败: %v", err)
	}
	_, assistantSeq, err := repo.AppendTurn(ctx, session.ID, "问题")
	if err != nil {
		t.Fatalf("AppendTurn 失败: %v", err)
	}
	chat := newTestChat(t, store)
	chat.mu.Lock()
	chat.active["dev_stale:claude"] = true
	chat.streams[streamKey(session.ID, assistantSeq)] = &streamBuffer{
		AgentID: "dev_stale:claude", DeviceID: "dev_stale", SessionID: session.ID,
		AssistantSeq: assistantSeq, Chunks: map[int]string{0: "半截"}, MaxIndex: 0,
	}
	chat.mu.Unlock()

	affected, err := chat.InterruptStaleStreaming(ctx, "")
	if err != nil {
		t.Fatalf("InterruptStaleStreaming 失败: %v", err)
	}
	if affected != 1 {
		t.Errorf("应兜底 1 条，实际 %d", affected)
	}
	var status string
	if err := store.DB.QueryRowContext(ctx,
		`SELECT status FROM messages WHERE session_id = ? AND seq = ?`,
		session.ID, assistantSeq).Scan(&status); err != nil {
		t.Fatalf("读取占位消息状态失败: %v", err)
	}
	if status != protocol.MessageStatusInterrupted {
		t.Errorf("残留占位应改为 interrupted，实际 %s", status)
	}
	chat.mu.Lock()
	_, streamLeft := chat.streams[streamKey(session.ID, assistantSeq)]
	activeLeft := chat.active["dev_stale:claude"]
	chat.mu.Unlock()
	if streamLeft || activeLeft {
		t.Error("兜底后应清内存缓冲与活跃 turn 标记")
	}
	// 幂等：第二次扫描不应再处理。
	again, err := chat.InterruptStaleStreaming(ctx, "dev_stale")
	if err != nil {
		t.Fatalf("二次 InterruptStaleStreaming 失败: %v", err)
	}
	if again != 0 {
		t.Errorf("二次扫描应无残留，实际 %d", again)
	}
}

// TestMapDoneStatus 校验 chat.done 终态到 messages.status 的映射（两套拼写不能混用）。
func TestMapDoneStatus(t *testing.T) {
	cases := map[string]string{
		protocol.DoneStatusCompleted:   protocol.MessageStatusComplete,
		protocol.DoneStatusStopped:     protocol.MessageStatusStopped,
		protocol.DoneStatusInterrupted: protocol.MessageStatusInterrupted,
		protocol.DoneStatusFailed:      protocol.MessageStatusError,
	}
	for done, want := range cases {
		if got := mapDoneStatus(done); got != want {
			t.Errorf("done=%s 期望 %s，实际 %s", done, want, got)
		}
	}
}
