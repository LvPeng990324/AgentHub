package server

// 回归用例（D-01）：tryBegin 原子占位之后，任一失败分支都必须释放槽位，
// 否则该 agent 会在下一次 chat.send 时被永久判定为「生成中」（4003）。

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/agenthub/agenthub/pkg/protocol"
)

func TestChatReleasesActiveSlotOnDBFailure(t *testing.T) {
	srv, ts := newTestServer(t)
	ctx := context.Background()

	agent := protocol.AgentSnapshot{
		Type: protocol.AgentTypeClaude, DisplayName: "Claude Code",
		Version: "1.0.0", ExecPath: "/usr/bin/claude", Status: protocol.AgentStatusAvailable,
	}
	_ = dialAdapterWS(t, ts, "dev_slot", agent)
	agentID := "dev_slot:claude"
	token := login(t, ts)
	if status, result := callAPI(t, ts, http.MethodPost,
		"/api/v1/agents/"+url.PathEscape(agentID)+"/register", token, nil); status != http.StatusOK || result.Code != protocol.CodeOK {
		t.Fatalf("注册失败: %d/%d", status, result.Code)
	}

	client := dialClientWS(t, ts, token)
	serverConn := serverSideClientConn(t, srv)

	// 关闭数据库，使 tryBegin 之后的第一次 DB 操作必然失败。
	if err := srv.store.DB.Close(); err != nil {
		t.Fatalf("关闭数据库失败: %v", err)
	}

	payload := protocol.ChatSendPayload{AgentID: agentID, Content: "第一次发送"}

	// 第一次：占位成功 → DB 失败 → 必须释放槽位，返回 5000（而不是 4003）。
	srv.chat.OnClientSend(ctx, serverConn, payload)
	frame, ok := readUntil(client, protocol.FrameChatError, 3*time.Second)
	if !ok {
		t.Fatal("DB 失败时应回 chat.error")
	}
	var first protocol.ChatErrorPayload
	if err := frame.DecodePayload(&first); err != nil {
		t.Fatalf("解析 chat.error 失败: %v", err)
	}
	if first.Code == protocol.CodeGenerationInProgress {
		t.Fatalf("首次失败不应返回 4003（说明占位过早判定），实际 %d", first.Code)
	}

	// 第二次：槽位若已释放，仍能进入 DB 阶段并再次返回 5000；泄漏则会一直 4003。
	srv.chat.OnClientSend(ctx, serverConn, payload)
	frame, ok = readUntil(client, protocol.FrameChatError, 3*time.Second)
	if !ok {
		t.Fatal("第二次发送也应回 chat.error")
	}
	var second protocol.ChatErrorPayload
	if err := frame.DecodePayload(&second); err != nil {
		t.Fatalf("解析 chat.error 失败: %v", err)
	}
	if second.Code == protocol.CodeGenerationInProgress {
		t.Errorf("槽位未释放：第二次发送被永久判定为生成中（4003）")
	}
	if second.Code != protocol.CodeInternalError {
		t.Errorf("DB 不可用时应返回 5000，实际 %d", second.Code)
	}

	srv.chat.mu.Lock()
	active := srv.chat.active[agentID]
	srv.chat.mu.Unlock()
	if active {
		t.Error("失败后活跃 turn 槽位必须释放")
	}
}
