package server

// 第 2 轮 QA 回归（D-01）：独立验证「同一 agent 单活跃 turn」的真实性——
// 不复用工程师用例，换并发度（10 路）、换时序（先成功后突发）、
// 并逐条验证 tryBegin 之后**每个失败分支都释放槽位**、成功路径也释放，
// 以及「设备不回收」窗口内槽位仅能由 InterruptStaleStreaming 释放这一事实。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// liveAgent 描述一套「设备在线 + agent 已注册 + 浏览器已连接」的测试现场。
type liveAgent struct {
	AgentID   string
	SessionID string
	DeviceID  string
	Adapter   *websocket.Conn
	Client    *websocket.Conn
	Token     string
}

// newLiveAgent 造一套真实在线环境：adapter WS 上报快照 → API 注册 → 浏览器 WS 建连。
func newLiveAgent(t *testing.T, srv *Server, ts *httptest.Server, deviceID string) liveAgent {
	t.Helper()
	agent := protocol.AgentSnapshot{
		Type: protocol.AgentTypeClaude, DisplayName: "Claude Code",
		Version: "1.0.0", ExecPath: "/usr/bin/claude", Status: protocol.AgentStatusAvailable,
	}
	adapter := dialAdapterWS(t, ts, deviceID, agent)
	agentID := deviceID + ":claude"

	token := login(t, ts)
	status, result := callAPI(t, ts, http.MethodPost,
		"/api/v1/agents/"+url.PathEscape(agentID)+"/register", token, nil)
	if status != http.StatusOK || result.Code != protocol.CodeOK {
		t.Fatalf("注册 %s 失败: %d/%d %s", agentID, status, result.Code, result.Message)
	}
	var registered AgentDTO
	if err := json.Unmarshal(result.Data, &registered); err != nil {
		t.Fatalf("解析注册结果失败: %v", err)
	}
	if registered.SessionID == nil {
		t.Fatal("注册后应返回 session_id")
	}

	client := dialClientWS(t, ts, token)
	if _, ok := readUntil(client, protocol.FrameClientReady, 3*time.Second); !ok {
		t.Fatal("浏览器 WS 未收到 client.ready")
	}
	return liveAgent{
		AgentID: agentID, SessionID: *registered.SessionID, DeviceID: deviceID,
		Adapter: adapter, Client: client, Token: token,
	}
}

// TestQA2ConcurrentSendTenWorkers 10 路并发 chat.send：只允许 1 次受理，其余 9 次 4003。
func TestQA2ConcurrentSendTenWorkers(t *testing.T) {
	srv, ts := newTestServer(t)
	env := newLiveAgent(t, srv, ts, "dev_ten")
	_ = env
	serverConn := serverSideClientConn(t, srv)
	agentID := "dev_ten:claude"

	const workers = 10
	payload := protocol.ChatSendPayload{AgentID: agentID, Content: "并发发送"}
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for index := 0; index < workers; index++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			srv.chat.OnClientSend(context.Background(), serverConn, payload)
		}()
	}
	start.Done()
	done.Wait()

	acks, inProgress, other := 0, 0, 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		frame, ok := readFrame(env.Client, time.Until(deadline))
		if !ok {
			break
		}
		switch frame.Type {
		case protocol.FrameChatSendAck:
			acks++
		case protocol.FrameChatError:
			var payload protocol.ChatErrorPayload
			if err := frame.DecodePayload(&payload); err == nil && payload.Code == protocol.CodeGenerationInProgress {
				inProgress++
				continue
			}
			other++
		}
	}

	srv.chat.mu.Lock()
	streamCount := len(srv.chat.streams)
	activeCount := len(srv.chat.active)
	srv.chat.mu.Unlock()

	if acks != 1 || inProgress != workers-1 || other != 0 {
		t.Errorf("10 路并发应 1×ack + %d×4003：实际 acks=%d 4003=%d 其它=%d", workers-1, acks, inProgress, other)
	}
	if streamCount != 1 || activeCount != 1 {
		t.Errorf("应只有 1 个流式缓冲与 1 个活跃槽位，实际 streams=%d active=%d", streamCount, activeCount)
	}
	// 库里只能有 1 轮（user + assistant 占位），不能出现多份占位。
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM messages`); got != 2 {
		t.Errorf("并发发送只应落 2 条消息（1 user + 1 assistant 占位），实际 %d", got)
	}
}

// TestQA2SequentialThenBurst 先成功 1 次（设备不回复，槽位保持占用），随后 5 路并发：
// 全部必须 4003，且不得产生任何新消息。
func TestQA2SequentialThenBurst(t *testing.T) {
	srv, ts := newTestServer(t)
	env := newLiveAgent(t, srv, ts, "dev_burst")
	serverConn := serverSideClientConn(t, srv)

	srv.chat.OnClientSend(context.Background(), serverConn,
		protocol.ChatSendPayload{AgentID: env.AgentID, Content: "第一轮"})
	if frame, ok := readUntil(env.Client, protocol.FrameChatSendAck, 3*time.Second); !ok || frame.Type != protocol.FrameChatSendAck {
		t.Fatal("首轮应收到 send_ack")
	}
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM messages`); got != 2 {
		t.Fatalf("首轮应落 2 条消息，实际 %d", got)
	}

	const burst = 5
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for index := 0; index < burst; index++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			srv.chat.OnClientSend(context.Background(), serverConn,
				protocol.ChatSendPayload{AgentID: env.AgentID, Content: "突发"})
		}()
	}
	start.Done()
	done.Wait()

	acks, inProgress := 0, 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		frame, ok := readFrame(env.Client, time.Until(deadline))
		if !ok {
			break
		}
		if frame.Type == protocol.FrameChatSendAck {
			acks++
			continue
		}
		if frame.Type == protocol.FrameChatError {
			var payload protocol.ChatErrorPayload
			if err := frame.DecodePayload(&payload); err == nil && payload.Code == protocol.CodeGenerationInProgress {
				inProgress++
			}
		}
	}
	if acks != 0 || inProgress != burst {
		t.Errorf("槽位占用期间突发 %d 路应全部 4003：实际 acks=%d 4003=%d", burst, acks, inProgress)
	}
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM messages`); got != 2 {
		t.Errorf("被拒的发送不得落库，仍应为 2 条，实际 %d", got)
	}
}

// TestQA2SlotReleasedOnFailureBranches tryBegin 之后的失败分支必须释放槽位：
// 每个分支连续调用**两次**，第二次必须仍是同一个业务错误码（而不是 4003 表示槽位泄漏）。
func TestQA2SlotReleasedOnFailureBranches(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(t *testing.T, srv *Server)
		agentID  string
		wantCode int
	}{
		{
			name:     "agent 不存在",
			setup:    func(t *testing.T, srv *Server) {},
			agentID:  "dev_ghost:claude",
			wantCode: protocol.CodeAgentNotFound,
		},
		{
			name: "agent 未注册",
			setup: func(t *testing.T, srv *Server) {
				insertFixture(t, srv.store, "dev_unreg", "dev_unreg:claude", protocol.AgentTypeClaude, 0)
				if _, err := srv.store.DB.Exec(
					`UPDATE agents SET registered = 0 WHERE id = ?`, "dev_unreg:claude"); err != nil {
					t.Fatalf("置未注册失败: %v", err)
				}
			},
			agentID:  "dev_unreg:claude",
			wantCode: protocol.CodeAgentNotRegistered,
		},
		{
			name: "agent 离线",
			setup: func(t *testing.T, srv *Server) {
				insertFixture(t, srv.store, "dev_offq", "dev_offq:claude", protocol.AgentTypeClaude, 0)
				if _, err := srv.store.DB.Exec(
					`UPDATE devices SET online = 0 WHERE id = ?`, "dev_offq"); err != nil {
					t.Fatalf("置设备离线失败: %v", err)
				}
				if _, err := srv.store.DB.Exec(
					`UPDATE agents SET online = 0 WHERE id = ?`, "dev_offq:claude"); err != nil {
					t.Fatalf("置 agent 离线失败: %v", err)
				}
			},
			agentID:  "dev_offq:claude",
			wantCode: protocol.CodeAgentOffline,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			srv, ts := newTestServer(t)
			testCase.setup(t, srv)
			token := login(t, ts)
			client := dialClientWS(t, ts, token)
			serverConn := serverSideClientConn(t, srv)
			_ = client

			for attempt := 1; attempt <= 2; attempt++ {
				srv.chat.OnClientSend(context.Background(), serverConn, protocol.ChatSendPayload{
					AgentID: testCase.agentID, Content: "测试",
				})
				frame, ok := readUntil(client, protocol.FrameChatError, 3*time.Second)
				if !ok {
					t.Fatalf("第 %d 次应回 chat.error", attempt)
				}
				var payload protocol.ChatErrorPayload
				if err := frame.DecodePayload(&payload); err != nil {
					t.Fatalf("解析 chat.error 失败: %v", err)
				}
				got := payload.Code
				if attempt == 1 && got != testCase.wantCode {
					t.Errorf("第 1 次应回 %d，实际 %d", testCase.wantCode, got)
				}
				if attempt == 2 {
					if got == protocol.CodeGenerationInProgress {
						t.Errorf("第 2 次不应是 4003（槽位未释放导致泄漏）：%+v", payload)
					}
					if got != testCase.wantCode {
						t.Errorf("第 2 次应仍为 %d（槽位已释放），实际 %d", testCase.wantCode, got)
					}
				}
			}
			srv.chat.mu.Lock()
			activeCount := len(srv.chat.active)
			srv.chat.mu.Unlock()
			if activeCount != 0 {
				t.Errorf("失败分支必须释放槽位，实际残留 %d 个", activeCount)
			}
		})
	}
}

// TestQA2SlotReleasedOnDBFailure DB 故障（连接被关闭）时必须回 5000 且释放槽位，
// 不能把该 agent 永久锁死在 4003。
func TestQA2SlotReleasedOnDBFailure(t *testing.T) {
	srv, ts := newTestServer(t)
	token := login(t, ts)
	client := dialClientWS(t, ts, token)
	serverConn := serverSideClientConn(t, srv)

	// 关闭数据库，制造 GetByID 失败。
	if err := srv.store.DB.Close(); err != nil {
		t.Fatalf("关闭测试数据库失败: %v", err)
	}

	srv.chat.OnClientSend(context.Background(), serverConn, protocol.ChatSendPayload{
		AgentID: "dev_any:claude", Content: "DB 已挂",
	})
	frame, ok := readUntil(client, protocol.FrameChatError, 3*time.Second)
	if !ok {
		t.Fatal("DB 故障应回 chat.error")
	}
	var payload protocol.ChatErrorPayload
	if err := frame.DecodePayload(&payload); err != nil {
		t.Fatalf("解析 chat.error 失败: %v", err)
	}
	if payload.Code != protocol.CodeInternalError {
		t.Errorf("DB 故障应回 5000，实际 %d", payload.Code)
	}
	srv.chat.mu.Lock()
	activeCount := len(srv.chat.active)
	srv.chat.mu.Unlock()
	if activeCount != 0 {
		t.Errorf("DB 失败分支必须释放槽位，实际残留 %d 个（该 agent 将永久 4003）", activeCount)
	}
}

// TestQA2SlotReleasedAfterCompletedTurn 成功路径：一轮对话正常完成后槽位必须释放，
// 下一次 send 能正常开始（不被 4003 拦住）。
func TestQA2SlotReleasedAfterCompletedTurn(t *testing.T) {
	srv, ts := newTestServer(t)
	env := newLiveAgent(t, srv, ts, "dev_ok")

	// 第一轮：浏览器发送 → 平台转发 → 设备回 chunk + done。
	writeFrame(t, env.Client, protocol.FrameChatSend, 1, protocol.ChatSendPayload{
		AgentID: env.AgentID, Content: "第一轮",
	})
	ackFrame, ok := readUntil(env.Client, protocol.FrameChatSendAck, 3*time.Second)
	if !ok {
		t.Fatal("第一轮未收到 send_ack")
	}
	var ack protocol.ChatSendAckPayload
	if err := ackFrame.DecodePayload(&ack); err != nil {
		t.Fatalf("解析 send_ack 失败: %v", err)
	}
	adapterConn := env.Adapter
	if _, ok := readUntil(adapterConn, protocol.FrameChatSend, 3*time.Second); !ok {
		t.Fatal("设备未收到转发的 chat.send")
	}
	writeFrame(t, adapterConn, protocol.FrameChatChunk, 2, protocol.ChatChunkPayload{
		AgentID: env.AgentID, SessionID: ack.SessionID, AssistantSeq: ack.AssistantSeq,
		ChunkIndex: 0, Delta: "完整回复",
	})
	writeFrame(t, adapterConn, protocol.FrameChatDone, 3, protocol.ChatDonePayload{
		AgentID: env.AgentID, SessionID: ack.SessionID, AssistantSeq: ack.AssistantSeq,
		Status: protocol.DoneStatusCompleted, ResumeID: "resume-round2",
	})
	if _, ok := readUntil(env.Client, protocol.FrameChatDone, 3*time.Second); !ok {
		t.Fatal("第一轮未收到 chat.done")
	}

	// 槽位应已释放。
	srv.chat.mu.Lock()
	activeAfterDone := len(srv.chat.active)
	streamsAfterDone := len(srv.chat.streams)
	srv.chat.mu.Unlock()
	if activeAfterDone != 0 || streamsAfterDone != 0 {
		t.Errorf("chat.done 后应释放槽位与缓冲，实际 active=%d streams=%d", activeAfterDone, streamsAfterDone)
	}

	// 第二轮必须能正常开始（拿到新的 seq=3/4 而不是 4003）。
	writeFrame(t, env.Client, protocol.FrameChatSend, 2, protocol.ChatSendPayload{
		AgentID: env.AgentID, Content: "第二轮",
	})
	secondAck, ok := readUntil(env.Client, protocol.FrameChatSendAck, 3*time.Second)
	if !ok {
		t.Fatal("第二轮未收到 send_ack（槽位可能未释放，被 4003 拦住）")
	}
	var ack2 protocol.ChatSendAckPayload
	if err := secondAck.DecodePayload(&ack2); err != nil {
		t.Fatalf("解析第二次 send_ack 失败: %v", err)
	}
	if ack2.UserSeq != ack.AssistantSeq+1 {
		t.Errorf("第二轮 user_seq 应接续上一轮 assistant_seq（%d），实际 %d", ack.AssistantSeq+1, ack2.UserSeq)
	}
	// 第一轮助手消息已完整落库。
	var content, status string
	if err := srv.store.DB.QueryRow(
		`SELECT content, status FROM messages WHERE session_id = ? AND seq = ?`,
		ack.SessionID, ack.AssistantSeq).Scan(&content, &status); err != nil {
		t.Fatalf("读取第一轮助手消息失败: %v", err)
	}
	if content != "完整回复" || status != protocol.MessageStatusComplete {
		t.Errorf("第一轮助手消息异常: content=%q status=%s", content, status)
	}
}

// TestQA2SlotHeldUntilStaleInterrupt 设备「收下不回收」（永不回 chat.done）时，
// 槽位保持占用（第二次 4003）；只有 InterruptStaleStreaming（离线/启动兜底）能释放。
// 该窗口是修复后的既有设计取舍：本轮把它固化为可观测事实，并确认有恢复路径。
func TestQA2SlotHeldUntilStaleInterrupt(t *testing.T) {
	srv, ts := newTestServer(t)
	env := newLiveAgent(t, srv, ts, "dev_hang")
	ctx := context.Background()

	writeFrame(t, env.Client, protocol.FrameChatSend, 1, protocol.ChatSendPayload{
		AgentID: env.AgentID, Content: "会挂住的一轮",
	})
	if _, ok := readUntil(env.Client, protocol.FrameChatSendAck, 3*time.Second); !ok {
		t.Fatal("未收到 send_ack")
	}

	// 第二次：槽位仍被占用 → 4003。
	writeFrame(t, env.Client, protocol.FrameChatSend, 2, protocol.ChatSendPayload{
		AgentID: env.AgentID, Content: "第二次",
	})
	frame, ok := readUntil(env.Client, protocol.FrameChatError, 3*time.Second)
	if !ok {
		t.Fatal("槽位占用期间应回 chat.error")
	}
	var payload protocol.ChatErrorPayload
	if err := frame.DecodePayload(&payload); err != nil {
		t.Fatalf("解析 chat.error 失败: %v", err)
	}
	if payload.Code != protocol.CodeGenerationInProgress {
		t.Errorf("应为 4003，实际 %d", payload.Code)
	}

	// 兜底中断（等价于设备离线/启动时扫描）后槽位必须释放。
	affected, err := srv.chat.InterruptStaleStreaming(ctx, env.DeviceID)
	if err != nil {
		t.Fatalf("InterruptStaleStreaming 失败: %v", err)
	}
	if affected != 1 {
		t.Errorf("应中断 1 条 streaming 占位，实际 %d", affected)
	}

	writeFrame(t, env.Client, protocol.FrameChatSend, 3, protocol.ChatSendPayload{
		AgentID: env.AgentID, Content: "兜底后重试",
	})
	acks, errors4003 := 0, 0
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && acks == 0 {
		frame, ok := readFrame(env.Client, time.Until(deadline))
		if !ok {
			break
		}
		switch frame.Type {
		case protocol.FrameChatSendAck:
			acks++
		case protocol.FrameChatError:
			var errPayload protocol.ChatErrorPayload
			if err := frame.DecodePayload(&errPayload); err == nil && errPayload.Code == protocol.CodeGenerationInProgress {
				errors4003++
			}
		}
	}
	if acks != 1 || errors4003 != 0 {
		t.Errorf("兜底中断后应能重新发送（acks=1），实际 acks=%d 4003=%d", acks, errors4003)
	}
}

// TestQA2ConflictCodeIsStable 连续 30 次「占用中发送」必须稳定返回 4003，
// 且中文文案不变（前端据此提示，不能漂移成 5000）。
func TestQA2ConflictCodeIsStable(t *testing.T) {
	srv, ts := newTestServer(t)
	env := newLiveAgent(t, srv, ts, "dev_stable")
	serverConn := serverSideClientConn(t, srv)

	srv.chat.OnClientSend(context.Background(), serverConn,
		protocol.ChatSendPayload{AgentID: env.AgentID, Content: "占位"})
	if _, ok := readUntil(env.Client, protocol.FrameChatSendAck, 3*time.Second); !ok {
		t.Fatal("首次发送应成功")
	}

	for attempt := 0; attempt < 30; attempt++ {
		srv.chat.OnClientSend(context.Background(), serverConn,
			protocol.ChatSendPayload{AgentID: env.AgentID, Content: fmt.Sprintf("第 %d 次", attempt)})
		frame, ok := readUntil(env.Client, protocol.FrameChatError, 3*time.Second)
		if !ok {
			t.Fatalf("第 %d 次应回 chat.error", attempt)
		}
		var payload protocol.ChatErrorPayload
		if err := frame.DecodePayload(&payload); err != nil {
			t.Fatalf("解析 chat.error 失败: %v", err)
		}
		if payload.Code != protocol.CodeGenerationInProgress {
			t.Fatalf("第 %d 次应为 4003，实际 %d", attempt, payload.Code)
		}
		if !strings.Contains(payload.Message, "生成中") {
			t.Errorf("4003 文案应含「生成中」，实际 %q", payload.Message)
		}
	}
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM messages`); got != 2 {
		t.Errorf("30 次被拒不得落库，仍应为 2 条，实际 %d", got)
	}
}
