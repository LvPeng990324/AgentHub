package server

// QA 补充测试（独立于工程师自测）：聚焦 §4.3 高风险区——
// E11 并发与事务、三层 Seq、WS 资源泄漏与背压、鉴权边界与日志脱敏、
// 静态资源路由、SQLite 并发/WAL/外键、备注边界、离线与在线恢复。
// 全部用例均以设计文档 §5~§7 与 PRD R-01~R-10 的验收口径为断言依据。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/agenthub/agenthub/internal/config"
	"github.com/agenthub/agenthub/pkg/protocol"
)

// ---------- QA 专用小工具 ----------

// rawCall 是可直接在 goroutine 中使用的 REST 调用（不调用 t.Fatalf）。
func rawCall(client *http.Client, method string, target string, token string, body any) (int, int, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, 0, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, target, reader)
	if err != nil {
		return 0, 0, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, 0, err
	}
	defer response.Body.Close()
	var envelope struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return response.StatusCode, 0, err
	}
	return response.StatusCode, envelope.Code, nil
}

// wsEndpoint 由 httptest 地址拼出 WS 地址。
func wsEndpoint(ts *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + path
}

// dialClientWS 登录后建立浏览器 WS 连接。
func dialClientWS(t *testing.T, ts *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	conn, response, err := websocket.DefaultDialer.Dial(wsEndpoint(ts, "/api/v1/ws/client?token="+url.QueryEscape(token)), nil)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("ws/client 握手失败 (HTTP %d): %v", status, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// dialAdapterWS 建立设备侧 WS 连接并发送 F01 全量快照，返回连接与 F02 ack。
func dialAdapterWS(t *testing.T, ts *httptest.Server, deviceID string, agents ...protocol.AgentSnapshot) *websocket.Conn {
	t.Helper()
	target := wsEndpoint(ts, fmt.Sprintf("/api/v1/ws/adapter?token=test-device-token&device_id=%s&protocol_version=%d",
		url.QueryEscape(deviceID), protocol.ProtocolVersion))
	conn, response, err := websocket.DefaultDialer.Dial(target, nil)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("ws/adapter 握手失败 (HTTP %d): %v", status, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	payload := protocol.DeviceRegisterPayload{
		DeviceID: deviceID, Name: "QA 设备 " + deviceID, OS: "linux", Arch: "amd64",
		AdapterVersion: "0.1.0", ProtocolVersion: protocol.ProtocolVersion, Agents: agents,
	}
	writeFrame(t, conn, protocol.FrameDeviceRegister, 1, payload)
	frame, ok := readFrame(conn, 3*time.Second)
	if !ok || frame.Type != protocol.FrameDeviceRegisterAck {
		t.Fatalf("未收到 F02 device.register_ack（实际 %+v ok=%v）", frame, ok)
	}
	return conn
}

// writeFrame 以连接级 seq 发送一帧。
func writeFrame(t *testing.T, conn *websocket.Conn, frameType string, seq int64, payload any) {
	t.Helper()
	message, err := protocol.NewMessage(frameType, seq, payload)
	if err != nil {
		t.Fatalf("构造帧 %s 失败: %v", frameType, err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, message.Encode()); err != nil {
		t.Fatalf("发送帧 %s 失败: %v", frameType, err)
	}
}

// readFrame 在超时内读一帧；超时或连接关闭返回 ok=false（不判失败，便于断言"无帧"）。
func readFrame(conn *websocket.Conn, timeout time.Duration) (protocol.Message, bool) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	_, data, err := conn.ReadMessage()
	if err != nil {
		return protocol.Message{}, false
	}
	var message protocol.Message
	if err := message.Decode(data); err != nil {
		return protocol.Message{}, false
	}
	return message, true
}

// readUntil 循环读帧直到命中目标类型或超时，返回命中的帧。
func readUntil(conn *websocket.Conn, frameType string, timeout time.Duration) (protocol.Message, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		frame, ok := readFrame(conn, time.Until(deadline))
		if !ok {
			return protocol.Message{}, false
		}
		if frame.Type == frameType {
			return frame, true
		}
	}
	return protocol.Message{}, false
}

// serverSideClientConn 取出服务端登记的那条浏览器 WSConn（同包可见，供并发调用编排器）。
func serverSideClientConn(t *testing.T, srv *Server) *WSConn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.hub.mu.RLock()
		for _, conn := range srv.hub.clients {
			srv.hub.mu.RUnlock()
			return conn
		}
		srv.hub.mu.RUnlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("服务端未登记任何浏览器 WS 连接")
	return nil
}

// syncBuffer 是并发安全的日志缓冲（供 K7 脱敏断言）。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ---------- 鉴权边界 ----------

// TestQACredentialSeparation 验证两套凭据彻底分离（D-2）：
// 设备令牌不能登录 REST / ws/client，登录 token 不能上 ws/adapter，登录密码不能当设备令牌。
func TestQACredentialSeparation(t *testing.T) {
	_, ts := newTestServer(t)

	// ① 设备令牌当登录密码 → 1002。
	status, result := callAPI(t, ts, http.MethodPost, "/api/v1/auth/login", "",
		LoginRequest{Password: "test-device-token"})
	if status != http.StatusUnauthorized || result.Code != protocol.CodeInvalidPassword {
		t.Errorf("用设备令牌登录应返回 1002/401，实际 %d/%d", status, result.Code)
	}

	// ② 登录密码当设备令牌 → ws/adapter 401 + 5001（Upgrade 之前拒绝）。
	adapterURL := wsEndpoint(ts, "/api/v1/ws/adapter?token=test-login-password&device_id=dev_x")
	conn, response, err := websocket.DefaultDialer.Dial(adapterURL, nil)
	if err == nil {
		_ = conn.Close()
		t.Fatal("用登录密码应拒绝 ws/adapter 握手，实际握手成功")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ws/adapter 拒绝登录密码应返回 401，实际 %+v", response)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !strings.Contains(string(body), fmt.Sprint(protocol.CodeAdapterUnauthorized)) {
		t.Errorf("ws/adapter 拒绝应为 5001 包络，实际 %s", string(body))
	}

	// ③ 登录 token 当设备令牌 → 同样 5001/401。
	token := login(t, ts)
	conn, response, err = websocket.DefaultDialer.Dial(
		wsEndpoint(ts, "/api/v1/ws/adapter?token="+url.QueryEscape(token)+"&device_id=dev_x"), nil)
	if err == nil {
		_ = conn.Close()
		t.Fatal("用登录 token 应拒绝 ws/adapter 握手，实际握手成功")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Errorf("ws/adapter 拒绝登录 token 应返回 401，实际 %+v", response)
	}

	// ④ 设备令牌当 Bearer 访问 REST → 1001/401。
	if status, result = callAPI(t, ts, http.MethodGet, "/api/v1/devices", "test-device-token", nil); status != http.StatusUnauthorized || result.Code != protocol.CodeUnauthorized {
		t.Errorf("设备令牌访问 REST 应返回 1001/401，实际 %d/%d", status, result.Code)
	}

	// ⑤ 设备令牌上 ws/client → 401。
	conn, response, err = websocket.DefaultDialer.Dial(
		wsEndpoint(ts, "/api/v1/ws/client?token=test-device-token"), nil)
	if err == nil {
		_ = conn.Close()
		t.Error("设备令牌应拒绝 ws/client 握手，实际握手成功")
	}
	if response != nil && response.StatusCode != http.StatusUnauthorized {
		t.Errorf("ws/client 拒绝设备令牌应返回 401，实际 %d", response.StatusCode)
	}
}

// TestQATokenAndPasswordNeverLogged 验证 K7 脱敏：日志中不出现明文 token / 设备令牌 / 登录密码。
func TestQATokenAndPasswordNeverLogged(t *testing.T) {
	capture := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(capture, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	_, ts := newTestServer(t)
	token := login(t, ts)
	// 触发 ws/client 的握手日志（含 maskToken）与请求日志。
	conn := dialClientWS(t, ts, token)
	_ = conn.Close()
	// 制造一次失败握手，确认失败日志也不泄露令牌。
	_, _, _ = websocket.DefaultDialer.Dial(wsEndpoint(ts, "/api/v1/ws/adapter?token="+url.QueryEscape(token)+"&device_id=dev_x"), nil)
	time.Sleep(150 * time.Millisecond)

	logged := capture.String()
	if logged == "" {
		t.Fatal("未捕获到任何日志，无法断言脱敏（测试自身失效）")
	}
	for label, secret := range map[string]string{
		"登录 token": token,
		"设备令牌":     "test-device-token",
		"登录密码":     "test-login-password",
	} {
		if strings.Contains(logged, secret) {
			t.Errorf("日志泄露了%s明文：%s", label, secret)
		}
	}
	if strings.Contains(logged, "token="+token) {
		t.Error("日志出现了带 token 的完整 WS URL（K7 要求只打 path）")
	}
}

// ---------- 静态资源路由 ----------

// TestQASPAFallbackAndAPINotFound 验证：
// 非 /api 的 SPA 路由（/agents、/chat/xxx）fallback 到 index.html 且 200；
// /api/v1/unknown 必须 404，绝不能 fallback 到 index.html。
func TestQASPAFallbackAndAPINotFound(t *testing.T) {
	_, ts := newTestServer(t)

	for _, path := range []string{"/", "/agents", "/devices", "/discover", "/chat/dev_x%3Aclaude"} {
		status, body := rawGet(t, ts, path)
		if status != http.StatusOK {
			t.Errorf("%s 期望 200，实际 %d", path, status)
			continue
		}
		if !looksLikeHTML(body) {
			t.Errorf("%s 应返回 SPA HTML，实际 %q", path, truncate(body, 120))
		}
	}

	for _, path := range []string{"/api/v1/unknown", "/api/v1/agents/x/unknown", "/api/v2/meta"} {
		status, body := rawGet(t, ts, path)
		if status != http.StatusNotFound {
			t.Errorf("%s 期望 404，实际 %d", path, status)
		}
		if looksLikeHTML(body) {
			t.Errorf("%s 不应 fallback 到 index.html（把未知 API 当页面返回）", path)
		}
	}
}

// rawGet 发起 GET 并返回状态码与响应体。
func rawGet(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	response, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取 %s 响应体失败: %v", path, err)
	}
	return response.StatusCode, string(body)
}

// looksLikeHTML 判断响应体是否是一份 HTML 页面。
func looksLikeHTML(body string) bool {
	lowered := strings.ToLower(body)
	return strings.Contains(lowered, "<!doctype html") || strings.Contains(lowered, "<div id=\"root\"")
}

// truncate 截断日志/响应体片段，避免测试输出过长。
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}

// ---------- E11 并发与事务 ----------

// TestQAConcurrentUnregisterSameAgent 并发对同一 agent 取消注册 16 次：
// 全部应为成功（幂等，不能 500 / database is locked），且最终库态一致。
func TestQAConcurrentUnregisterSameAgent(t *testing.T) {
	srv, ts := newTestServer(t)
	ctx := context.Background()
	insertFixture(t, srv.store, "dev_race", "dev_race:claude", protocol.AgentTypeClaude, 4)
	token := login(t, ts)
	endpoint := ts.URL + "/api/v1/agents/" + url.PathEscape("dev_race:claude") + "/register"

	const workers = 16
	statuses := make([]int, workers)
	codes := make([]int, workers)
	errs := make([]error, workers)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for index := 0; index < workers; index++ {
		done.Add(1)
		go func(slot int) {
			defer done.Done()
			start.Wait()
			statuses[slot], codes[slot], errs[slot] = rawCall(ts.Client(), http.MethodDelete, endpoint, token, nil)
		}(index)
	}
	start.Done()
	done.Wait()

	for slot := 0; slot < workers; slot++ {
		if errs[slot] != nil {
			t.Fatalf("并发取消注册 #%d 请求失败: %v", slot, errs[slot])
		}
		if statuses[slot] != http.StatusOK || codes[slot] != protocol.CodeOK {
			t.Errorf("并发取消注册 #%d 应为 200/0（幂等），实际 %d/%d", slot, statuses[slot], codes[slot])
		}
	}

	// 库级终态：sessions/messages 清空，devices/agents 行与 remark/online 保留。
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM sessions`); got != 0 {
		t.Errorf("sessions 应清空，实际 %d", got)
	}
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM messages`); got != 0 {
		t.Errorf("messages 应级联清空，实际 %d", got)
	}
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM agents`); got != 1 {
		t.Errorf("agents 行应保留，实际 %d", got)
	}
	var registered, online int
	var remark string
	if err := srv.store.DB.QueryRowContext(ctx,
		`SELECT registered, online, remark FROM agents WHERE id = ?`, "dev_race:claude").
		Scan(&registered, &online, &remark); err != nil {
		t.Fatalf("读取 agent 终态失败: %v", err)
	}
	if registered != 0 {
		t.Errorf("registered 应为 0，实际 %d", registered)
	}
	if remark != "主力" {
		t.Errorf("remark 应保留，实际 %q", remark)
	}
	if online != 1 {
		t.Errorf("取消注册不得改动 online（K11），实际 %d", online)
	}
}

// TestQAUnregisterDuringInFlightTurn 取消注册与「正在生成的一轮对话」并发：
// 事务必须整体回滚/清空，内存缓冲不得残留，设备必须收到 F12，且迟到的 chunk/done 不能复活消息。
func TestQAUnregisterDuringInFlightTurn(t *testing.T) {
	srv, ts := newTestServer(t)

	agent := protocol.AgentSnapshot{
		Type: protocol.AgentTypeClaude, DisplayName: "Claude Code",
		Version: "1.0.0", ExecPath: "/usr/bin/claude", Status: protocol.AgentStatusAvailable,
	}
	adapter := dialAdapterWS(t, ts, "dev_live", agent)
	agentID := "dev_live:claude"

	token := login(t, ts)
	status, result := callAPI(t, ts, http.MethodPost, "/api/v1/agents/"+url.PathEscape(agentID)+"/register", token, nil)
	if status != http.StatusOK || result.Code != protocol.CodeOK {
		t.Fatalf("注册失败: %d/%d %s", status, result.Code, result.Message)
	}
	var registered AgentDTO
	if err := json.Unmarshal(result.Data, &registered); err != nil {
		t.Fatalf("解析注册结果失败: %v", err)
	}
	if registered.SessionID == nil {
		t.Fatal("注册后应返回 session_id")
	}
	sessionID := *registered.SessionID

	client := dialClientWS(t, ts, token)
	writeFrame(t, client, protocol.FrameChatSend, 1, protocol.ChatSendPayload{
		AgentID: agentID, Content: "开始生成一条很长的回复",
	})
	ackFrame, ok := readUntil(client, protocol.FrameChatSendAck, 3*time.Second)
	if !ok {
		t.Fatal("未收到 chat.send_ack")
	}
	var ack protocol.ChatSendAckPayload
	if err := ackFrame.DecodePayload(&ack); err != nil {
		t.Fatalf("解析 send_ack 失败: %v", err)
	}
	assistantSeq := ack.AssistantSeq

	// 设备侧应收到转发的 chat.send（不回复，模拟生成中）。
	if _, ok := readUntil(adapter, protocol.FrameChatSend, 3*time.Second); !ok {
		t.Fatal("设备未收到转发的 chat.send")
	}

	// 生成中执行取消注册。
	if status, result = callAPI(t, ts, http.MethodDelete, "/api/v1/agents/"+url.PathEscape(agentID)+"/register", token, nil); status != http.StatusOK || result.Code != protocol.CodeOK {
		t.Fatalf("取消注册失败: %d/%d", status, result.Code)
	}

	// 设备必须收到 F12 agent.unregistered（清内存 session id 与子进程）。
	if _, ok := readUntil(adapter, protocol.FrameAgentUnregistered, 3*time.Second); !ok {
		t.Error("取消注册后设备未收到 F12 agent.unregistered")
	}

	// 库态：会话与消息清零，agent 行保留。
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM sessions`); got != 0 {
		t.Errorf("sessions 应清空，实际 %d", got)
	}
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM messages`); got != 0 {
		t.Errorf("messages 应清空，实际 %d", got)
	}

	// 内存态：流式缓冲与活跃标记必须被清掉（否则会越积越多）。
	srv.chat.mu.Lock()
	streamCount := len(srv.chat.streams)
	active := srv.chat.active[agentID]
	srv.chat.mu.Unlock()
	if streamCount != 0 {
		t.Errorf("取消注册后流式缓冲应清空，实际残留 %d 条", streamCount)
	}
	if active {
		t.Error("取消注册后活跃 turn 标记应清除")
	}

	// 迟到的 chunk / done 不得让消息复活。
	writeFrame(t, adapter, protocol.FrameChatChunk, 9, protocol.ChatChunkPayload{
		AgentID: agentID, SessionID: sessionID, AssistantSeq: assistantSeq, ChunkIndex: 0, Delta: "迟到增量",
	})
	writeFrame(t, adapter, protocol.FrameChatDone, 10, protocol.ChatDonePayload{
		AgentID: agentID, SessionID: sessionID, AssistantSeq: assistantSeq,
		Status: protocol.DoneStatusCompleted, ResumeID: "late-resume",
	})
	time.Sleep(200 * time.Millisecond)
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM messages`); got != 0 {
		t.Errorf("迟到的 chunk/done 不得复活消息，实际 messages=%d", got)
	}
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM sessions`); got != 0 {
		t.Errorf("迟到的 chat.done 不得重建会话，实际 sessions=%d", got)
	}

	// 取消注册后取历史 → 3002。
	if _, result = callAPI(t, ts, http.MethodGet, "/api/v1/agents/"+url.PathEscape(agentID)+"/messages", token, nil); result.Code != protocol.CodeAgentNotRegistered {
		t.Errorf("未注册取历史应返回 3002，实际 %d", result.Code)
	}
}

// TestQAConcurrentSendKeepsSingleActiveTurn 同一 agent 并发 6 次 chat.send（U1）：
// 必须只有 1 次被受理，其余全部 4003 GENERATION_IN_PROGRESS，且只产生 1 条助手占位。
func TestQAConcurrentSendKeepsSingleActiveTurn(t *testing.T) {
	srv, ts := newTestServer(t)
	ctx := context.Background()

	agent := protocol.AgentSnapshot{
		Type: protocol.AgentTypeClaude, DisplayName: "Claude Code",
		Version: "1.0.0", ExecPath: "/usr/bin/claude", Status: protocol.AgentStatusAvailable,
	}
	adapter := dialAdapterWS(t, ts, "dev_conc", agent)
	agentID := "dev_conc:claude"

	token := login(t, ts)
	if status, result := callAPI(t, ts, http.MethodPost, "/api/v1/agents/"+url.PathEscape(agentID)+"/register", token, nil); status != http.StatusOK || result.Code != protocol.CodeOK {
		t.Fatalf("注册失败: %d/%d", status, result.Code)
	}
	// 让设备保持"不回复"，使第一轮长期处于 active 状态。
	_ = adapter
	client := dialClientWS(t, ts, token)
	serverConn := serverSideClientConn(t, srv)

	const workers = 6
	payload := protocol.ChatSendPayload{AgentID: agentID, Content: "并发发送"}
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for index := 0; index < workers; index++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			srv.chat.OnClientSend(ctx, serverConn, payload)
		}()
	}
	start.Done()
	done.Wait()

	acks, inProgress, other := 0, 0, 0
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		frame, ok := readFrame(client, time.Until(deadline))
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

	// 会话内只能有 1 条 user + 1 条 assistant 占位。
	srv.chat.mu.Lock()
	streamCount := len(srv.chat.streams)
	srv.chat.mu.Unlock()

	if acks != 1 || inProgress != workers-1 || other != 0 || streamCount != 1 {
		t.Errorf("同一 agent 并发发送必须只受理 1 次（U1/4003）："+
			"实际 acks=%d 4003=%d 其它错误=%d 流式缓冲=%d（期望 1/%d/0/1）",
			acks, inProgress, other, streamCount, workers-1)
	}
}

// ---------- 三层 Seq ----------

// TestQADuplicateAndStaleFramesDropped 验证 L1 连接级 seq 去重：
// 重复帧与回退帧必须被丢弃（心跳不再刷新 last_seen_at），递增帧正常处理。
//
// 注意：gorilla/websocket 一旦读超时，连接即不可再用，因此「无帧」断言
// 不能靠读超时实现，这里改用 DB 副作用（心跳刷新的 last_seen_at）作为观测量。
func TestQADuplicateAndStaleFramesDropped(t *testing.T) {
	srv, ts := newTestServer(t)
	ctx := context.Background()
	agent := protocol.AgentSnapshot{
		Type: protocol.AgentTypeClaude, DisplayName: "Claude Code",
		Version: "1.0.0", ExecPath: "/usr/bin/claude", Status: protocol.AgentStatusAvailable,
	}
	adapter := dialAdapterWS(t, ts, "dev_seq", agent)
	const sentinel = "2000-01-01T00:00:00Z"
	resetLastSeen := func() {
		t.Helper()
		if _, err := srv.store.DB.ExecContext(ctx,
			`UPDATE agents SET last_seen_at = ? WHERE id = ?`, sentinel, "dev_seq:claude"); err != nil {
			t.Fatalf("重置 last_seen_at 失败: %v", err)
		}
	}
	lastSeen := func() string {
		t.Helper()
		var value string
		if err := srv.store.DB.QueryRowContext(ctx,
			`SELECT last_seen_at FROM agents WHERE id = ?`, "dev_seq:claude").Scan(&value); err != nil {
			t.Fatalf("读取 last_seen_at 失败: %v", err)
		}
		return value
	}
	heartbeat := func(seq int64) {
		t.Helper()
		writeFrame(t, adapter, protocol.FrameDeviceHeartbeat, seq, protocol.HeartbeatPayload{
			DeviceID: "dev_seq",
			Agents:   []protocol.HeartbeatAgent{{Type: protocol.AgentTypeClaude, Status: protocol.AgentStatusAvailable}},
		})
	}

	// ① 首次心跳 seq=5：应处理并回 F04。
	resetLastSeen()
	heartbeat(5)
	if _, ok := readUntil(adapter, protocol.FrameHeartbeatAck, 2*time.Second); !ok {
		t.Fatal("首次心跳（seq=5）应回 F04 heartbeat_ack")
	}
	if lastSeen() == sentinel {
		t.Fatal("合法心跳应刷新 agent 的 last_seen_at")
	}

	// ② 重复 seq=5：必须丢弃（last_seen_at 不被刷新）。
	resetLastSeen()
	heartbeat(5)
	time.Sleep(300 * time.Millisecond)
	if got := lastSeen(); got != sentinel {
		t.Errorf("重复 seq 的帧未被丢弃（last_seen_at 被刷新为 %s）", got)
	}

	// ③ 回退 seq=3：必须丢弃。
	resetLastSeen()
	heartbeat(3)
	time.Sleep(300 * time.Millisecond)
	if got := lastSeen(); got != sentinel {
		t.Errorf("乱序旧帧（seq 回退）未被丢弃（last_seen_at 被刷新为 %s）", got)
	}

	// ④ 递增 seq=6：正常处理并回 ack。
	heartbeat(6)
	if _, ok := readUntil(adapter, protocol.FrameHeartbeatAck, 2*time.Second); !ok {
		t.Error("seq=6 的合法帧应正常处理并回 ack")
	}
	if got := lastSeen(); got == sentinel {
		t.Error("seq=6 合法心跳应刷新 last_seen_at")
	}
}

// ---------- 对话前置校验 ----------

// TestQAOfflineSendRejectedWithChineseHint 设备离线时 chat.send 必须拒发并回 3003 中文提示（R-08 / R-10）。
func TestQAOfflineSendRejectedWithChineseHint(t *testing.T) {
	srv, ts := newTestServer(t)
	ctx := context.Background()

	// 造一个"已注册但设备离线"的 agent（无 adapter WS 连接）。
	insertFixture(t, srv.store, "dev_off", "dev_off:claude", protocol.AgentTypeClaude, 0)
	if _, err := srv.store.DB.ExecContext(ctx,
		`UPDATE devices SET online = 0 WHERE id = ?`, "dev_off"); err != nil {
		t.Fatalf("置设备离线失败: %v", err)
	}
	if _, err := srv.store.DB.ExecContext(ctx,
		`UPDATE agents SET online = 0 WHERE id = ?`, "dev_off:claude"); err != nil {
		t.Fatalf("置 agent 离线失败: %v", err)
	}

	token := login(t, ts)
	client := dialClientWS(t, ts, token)
	writeFrame(t, client, protocol.FrameChatSend, 1, protocol.ChatSendPayload{
		AgentID: "dev_off:claude", Content: "离线还能发吗",
	})
	frame, ok := readUntil(client, protocol.FrameChatError, 3*time.Second)
	if !ok {
		t.Fatal("离线发送应回 chat.error")
	}
	var payload protocol.ChatErrorPayload
	if err := frame.DecodePayload(&payload); err != nil {
		t.Fatalf("解析 chat.error 失败: %v", err)
	}
	if payload.Code != protocol.CodeAgentOffline {
		t.Errorf("离线发送应回 3003，实际 %d", payload.Code)
	}
	if !strings.Contains(payload.Message, "当前离线") {
		t.Errorf("离线提示必须是可读中文（R-18），实际 %q", payload.Message)
	}
	// 未注册 / 不存在 agent 的错误码也要正确。
	writeFrame(t, client, protocol.FrameChatSend, 2, protocol.ChatSendPayload{
		AgentID: "dev_none:claude", Content: "不存在的 agent",
	})
	if frame, ok = readUntil(client, protocol.FrameChatError, 3*time.Second); !ok {
		t.Fatal("不存在的 agent 应回 chat.error")
	}
	if err := frame.DecodePayload(&payload); err != nil {
		t.Fatalf("解析 chat.error 失败: %v", err)
	}
	if payload.Code != protocol.CodeAgentNotFound {
		t.Errorf("不存在的 agent 应回 3001，实际 %d", payload.Code)
	}

	// 超长 prompt（> 64KiB）→ 4002。
	writeFrame(t, client, protocol.FrameChatSend, 3, protocol.ChatSendPayload{
		AgentID: "dev_off:claude", Content: strings.Repeat("啊", protocol.MaxPromptBytes),
	})
	if frame, ok = readUntil(client, protocol.FrameChatError, 3*time.Second); !ok {
		t.Fatal("超长 prompt 应回 chat.error")
	}
	if err := frame.DecodePayload(&payload); err != nil {
		t.Fatalf("解析 chat.error 失败: %v", err)
	}
	if payload.Code != protocol.CodeMessageTooLarge {
		t.Errorf("超长 prompt 应回 4002，实际 %d", payload.Code)
	}
}

// ---------- WS 资源与背压 ----------

// TestQAWSChurnNoGoroutineLeak 反复建连/断连 50 次，goroutine 数必须回落（无每连接泄漏）。
func TestQAWSChurnNoGoroutineLeak(t *testing.T) {
	srv, ts := newTestServer(t)
	token := login(t, ts)

	// 预热，避免把首次 TLS/DB 等一次性 goroutine 计入基线。
	warm := dialClientWS(t, ts, token)
	_ = warm.Close()
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	for index := 0; index < 50; index++ {
		conn := dialClientWS(t, ts, token)
		if _, ok := readFrame(conn, 2*time.Second); !ok {
			t.Fatalf("第 %d 次建连未收到 client.ready", index)
		}
		_ = conn.Close()
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+5 && srv.hub.ClientCount() == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("50 次建连/断连后资源未回收：goroutine 基线 %d → 当前 %d，hub 残留连接 %d",
		baseline, runtime.NumGoroutine(), srv.hub.ClientCount())
}

// TestQATrySendNonBlockingWhenBufferFull 验证 K9 背压：send 缓冲满时 TrySend
// 必须立即返回 false（不阻塞、不 panic），Close 之后再写也不 panic。
func TestQATrySendNonBlockingWhenBufferFull(t *testing.T) {
	conn := &WSConn{
		ID:   "qa_fake_conn",
		Kind: ConnKindClient,
		send: make(chan []byte, 4),
		done: make(chan struct{}),
	}
	message, err := protocol.NewMessage(protocol.FrameAgentStatus, 1, protocol.AgentStatusPayload{Reason: "qa"})
	if err != nil {
		t.Fatalf("构造帧失败: %v", err)
	}

	accepted := 0
	start := time.Now()
	for index := 0; index < 50; index++ {
		if conn.TrySend(message) {
			accepted++
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("缓冲满时 TrySend 阻塞了 %s（必须非阻塞）", elapsed)
	}
	if accepted != 4 {
		t.Errorf("send 缓冲容量 4，应只接受 4 帧，实际 %d", accepted)
	}

	// 关闭后再次下发：必须返回 false 且不 panic。
	close(conn.done)
	if conn.TrySend(message) {
		t.Error("连接已关闭后 TrySend 应返回 false")
	}
}

// ---------- 在线状态（R-10） ----------

// TestQADeviceOfflineThenRecover 验证离线判定动作与恢复广播：
// 置离线 → 设备与其下全部 agent online=0 + 广播 device.status / agent.status(device_offline)
// → 收到心跳后自动回到在线并广播 device_online（无需人工刷新）。
func TestQADeviceOfflineThenRecover(t *testing.T) {
	srv, ts := newTestServer(t)
	ctx := context.Background()

	agent := protocol.AgentSnapshot{
		Type: protocol.AgentTypeClaude, DisplayName: "Claude Code",
		Version: "1.0.0", ExecPath: "/usr/bin/claude", Status: protocol.AgentStatusAvailable,
	}
	adapter := dialAdapterWS(t, ts, "dev_presence", agent)
	token := login(t, ts)
	client := dialClientWS(t, ts, token)
	if _, ok := readUntil(client, protocol.FrameClientReady, 3*time.Second); !ok {
		t.Fatal("未收到 client.ready")
	}

	// 触发离线判定（等价于 45s 无心跳扫描命中）。
	if err := srv.hub.markOffline(ctx, "dev_presence"); err != nil {
		t.Fatalf("markOffline 失败: %v", err)
	}
	statusFrame, ok := readUntil(client, protocol.FrameAgentStatus, 3*time.Second)
	if !ok {
		t.Fatal("离线后应广播 agent.status")
	}
	var status protocol.AgentStatusPayload
	if err := statusFrame.DecodePayload(&status); err != nil {
		t.Fatalf("解析 agent.status 失败: %v", err)
	}
	if status.Reason != protocol.ReasonDeviceOffline {
		t.Errorf("离线广播 reason 应为 device_offline，实际 %q", status.Reason)
	}
	if len(status.Agents) != 1 || status.Agents[0].Online {
		t.Errorf("离线广播应包含 online=false 的 agent，实际 %+v", status.Agents)
	}
	if frame, ok := readUntil(client, protocol.FrameDeviceStatus, 3*time.Second); !ok {
		t.Error("离线后应广播 device.status")
	} else {
		var deviceStatus protocol.DeviceStatusPayload
		if err := frame.DecodePayload(&deviceStatus); err == nil && deviceStatus.Online {
			t.Error("device.status 应为 online=false")
		}
	}

	var deviceOnline, agentOnline int
	if err := srv.store.DB.QueryRowContext(ctx, `SELECT online FROM devices WHERE id = ?`, "dev_presence").Scan(&deviceOnline); err != nil {
		t.Fatalf("读取设备在线态失败: %v", err)
	}
	if err := srv.store.DB.QueryRowContext(ctx, `SELECT online FROM agents WHERE id = ?`, "dev_presence:claude").Scan(&agentOnline); err != nil {
		t.Fatalf("读取 agent 在线态失败: %v", err)
	}
	if deviceOnline != 0 || agentOnline != 0 {
		t.Errorf("离线后 devices/agents 均应 online=0，实际 %d/%d", deviceOnline, agentOnline)
	}

	// 恢复：设备发一次心跳 → 回在线并广播 device_online。
	writeFrame(t, adapter, protocol.FrameDeviceHeartbeat, 2, protocol.HeartbeatPayload{
		DeviceID: "dev_presence",
		Agents:   []protocol.HeartbeatAgent{{Type: protocol.AgentTypeClaude, Status: protocol.AgentStatusAvailable}},
	})
	if _, ok := readUntil(client, protocol.FrameAgentStatus, 3*time.Second); !ok {
		t.Error("设备恢复后应广播 agent.status(device_online)")
	}
	if err := srv.store.DB.QueryRowContext(ctx, `SELECT online FROM devices WHERE id = ?`, "dev_presence").Scan(&deviceOnline); err != nil {
		t.Fatalf("读取设备在线态失败: %v", err)
	}
	if err := srv.store.DB.QueryRowContext(ctx, `SELECT online FROM agents WHERE id = ?`, "dev_presence:claude").Scan(&agentOnline); err != nil {
		t.Fatalf("读取 agent 在线态失败: %v", err)
	}
	if deviceOnline != 1 || agentOnline != 1 {
		t.Errorf("心跳恢复后 devices/agents 应回到 online=1，实际 %d/%d", deviceOnline, agentOnline)
	}
}

// ---------- SQLite ----------

// TestQASQLiteConcurrentWritesNoLock 并发写不出现 "database is locked"，
// 且 PRAGMA（WAL / foreign_keys / busy_timeout）真实生效、WAL 文件已生成。
func TestQASQLiteConcurrentWritesNoLock(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	sessionID := insertFixture(t, store, "dev_w", "dev_w:claude", protocol.AgentTypeClaude, 0)
	repo := NewMessageRepo(store)

	var journalMode string
	if err := store.DB.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("读取 journal_mode 失败: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("journal_mode 应为 wal，实际 %q", journalMode)
	}
	enabled, err := store.ForeignKeysEnabled(ctx)
	if err != nil {
		t.Fatalf("读取 foreign_keys 失败: %v", err)
	}
	if !enabled {
		t.Error("PRAGMA foreign_keys 必须为 ON（级联清空依赖它）")
	}
	var busyTimeout int
	if err := store.DB.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("读取 busy_timeout 失败: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout 应为 5000，实际 %d", busyTimeout)
	}

	const writers = 24
	errCh := make(chan error, writers)
	var group sync.WaitGroup
	for index := 1; index <= writers; index++ {
		group.Add(1)
		go func(seq int) {
			defer group.Done()
			errCh <- repo.AppendMessage(ctx, sessionID, int64(seq), protocol.RoleUser, "并发写入", protocol.MessageStatusComplete)
		}(index)
	}
	group.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Errorf("并发写入失败（疑似 database is locked）: %v", err)
		}
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM messages`); got != writers {
		t.Errorf("并发写入应全部落库，期望 %d 条，实际 %d", writers, got)
	}

	// WAL 文件应出现在数据库旁（-wal）。
	if _, err := os.Stat(store.Path + "-wal"); err != nil {
		t.Errorf("未生成 WAL 文件 %s: %v", store.Path+"-wal", err)
	}
}

// ---------- 备注（R-05） ----------

// TestQARemarkBoundsAndIndependence 备注 ≤500 字、超限 400、
// 设备备注与 agent 备注互不影响，且备注不影响 registered / online。
func TestQARemarkBoundsAndIndependence(t *testing.T) {
	srv, ts := newTestServer(t)
	ctx := context.Background()
	insertFixture(t, srv.store, "dev_rm", "dev_rm:claude", protocol.AgentTypeClaude, 2)
	token := login(t, ts)

	exactly500 := strings.Repeat("备", protocol.MaxRemarkRunes)
	status, result := callAPI(t, ts, http.MethodPatch, "/api/v1/devices/dev_rm/remark", token,
		RemarkRequest{Remark: exactly500})
	if status != http.StatusOK || result.Code != protocol.CodeOK {
		t.Fatalf("500 字备注应接受，实际 %d/%d %s", status, result.Code, result.Message)
	}

	tooLong := strings.Repeat("注", protocol.MaxRemarkRunes+1)
	if status, result = callAPI(t, ts, http.MethodPatch, "/api/v1/devices/dev_rm/remark", token,
		RemarkRequest{Remark: tooLong}); status != http.StatusBadRequest || result.Code != protocol.CodeBadRequest {
		t.Errorf("501 字设备备注应返回 1004/400，实际 %d/%d", status, result.Code)
	}
	agentPath := "/api/v1/agents/" + url.PathEscape("dev_rm:claude") + "/remark"
	if status, result = callAPI(t, ts, http.MethodPatch, agentPath, token,
		RemarkRequest{Remark: tooLong}); status != http.StatusBadRequest || result.Code != protocol.CodeBadRequest {
		t.Errorf("501 字 agent 备注应返回 1004/400，实际 %d/%d", status, result.Code)
	}

	// 分别持久化且互不覆盖。
	if status, result = callAPI(t, ts, http.MethodPatch, agentPath, token,
		RemarkRequest{Remark: "主力开发 agent"}); status != http.StatusOK {
		t.Fatalf("设置 agent 备注失败: %d/%d", status, result.Code)
	}
	var deviceRemark, agentRemark string
	var registered, online int
	if err := srv.store.DB.QueryRowContext(ctx, `SELECT remark FROM devices WHERE id = ?`, "dev_rm").Scan(&deviceRemark); err != nil {
		t.Fatalf("读取设备备注失败: %v", err)
	}
	if err := srv.store.DB.QueryRowContext(ctx,
		`SELECT remark, registered, online FROM agents WHERE id = ?`, "dev_rm:claude").
		Scan(&agentRemark, &registered, &online); err != nil {
		t.Fatalf("读取 agent 备注失败: %v", err)
	}
	if deviceRemark != exactly500 {
		t.Errorf("设备备注应保持 500 字原文，实际长度 %d", len([]rune(deviceRemark)))
	}
	if agentRemark != "主力开发 agent" {
		t.Errorf("agent 备注未持久化，实际 %q", agentRemark)
	}
	if registered != 1 || online != 1 {
		t.Errorf("备注不得影响 registered/online，实际 %d/%d", registered, online)
	}

	// 刷新（重新拉列表）后备注仍在，且未注册的 agent 行也保留备注。
	if _, result = callAPI(t, ts, http.MethodPost, "/api/v1/agents/"+url.PathEscape("dev_rm:claude")+"/register", token, nil); result.Code != protocol.CodeOK {
		t.Fatalf("注册失败: %d", result.Code)
	}
	if _, result = callAPI(t, ts, http.MethodDelete, "/api/v1/agents/"+url.PathEscape("dev_rm:claude")+"/register", token, nil); result.Code != protocol.CodeOK {
		t.Fatalf("取消注册失败: %d", result.Code)
	}
	var discovered AgentListData
	_, result = callAPI(t, ts, http.MethodGet, "/api/v1/agents/discovered", token, nil)
	if err := json.Unmarshal(result.Data, &discovered); err != nil {
		t.Fatalf("解析 discovered 失败: %v", err)
	}
	if len(discovered.Items) != 1 || discovered.Items[0].Remark != "主力开发 agent" {
		t.Errorf("取消注册后备注应保留，实际 %+v", discovered.Items)
	}
}

// ---------- 列表与历史（R-07 / R-16） ----------

// TestQAListsAndHistoryCursor 只返回已注册 agent；since_seq 游标补齐不重复不丢序。
func TestQAListsAndHistoryCursor(t *testing.T) {
	srv, ts := newTestServer(t)
	ctx := context.Background()
	insertFixture(t, srv.store, "dev_l", "dev_l:claude", protocol.AgentTypeClaude, 2)
	if _, err := srv.store.DB.ExecContext(ctx,
		`UPDATE agents SET registered = 0 WHERE id = ?`, "dev_l:claude"); err != nil {
		t.Fatalf("置未注册失败: %v", err)
	}
	// 同设备第二个 agent（未注册）。
	if err := srv.agentRepo.UpsertSnapshot(ctx, "dev_l", []protocol.AgentSnapshot{{
		Type: protocol.AgentTypeKimi, DisplayName: "Kimi Code",
		Version: "2.0.0", ExecPath: "/usr/bin/kimi", Status: protocol.AgentStatusAvailable,
	}}); err != nil {
		t.Fatalf("写入 kimi 快照失败: %v", err)
	}
	token := login(t, ts)

	var registered AgentListData
	_, result := callAPI(t, ts, http.MethodGet, "/api/v1/agents", token, nil)
	if err := json.Unmarshal(result.Data, &registered); err != nil {
		t.Fatalf("解析已注册列表失败: %v", err)
	}
	if registered.Total != 0 || len(registered.Items) != 0 {
		t.Errorf("E09 只应返回 registered=1，实际 %+v", registered)
	}

	var discovered AgentListData
	_, result = callAPI(t, ts, http.MethodGet, "/api/v1/agents/discovered", token, nil)
	if err := json.Unmarshal(result.Data, &discovered); err != nil {
		t.Fatalf("解析已发现列表失败: %v", err)
	}
	if discovered.Total != 2 {
		t.Errorf("E08 应包含未注册 agent（2 条），实际 %d", discovered.Total)
	}
	for _, item := range discovered.Items {
		if item.DeviceName == "" || item.Type == "" || item.DisplayName == "" {
			t.Errorf("列表四要素缺失（名称/类型/所属设备）: %+v", item)
		}
	}

	// 注册后 E09 只返回 1 条。
	if _, result = callAPI(t, ts, http.MethodPost, "/api/v1/agents/"+url.PathEscape("dev_l:claude")+"/register", token, nil); result.Code != protocol.CodeOK {
		t.Fatalf("注册失败: %d", result.Code)
	}
	_, result = callAPI(t, ts, http.MethodGet, "/api/v1/agents", token, nil)
	if err := json.Unmarshal(result.Data, &registered); err != nil {
		t.Fatalf("解析已注册列表失败: %v", err)
	}
	if registered.Total != 1 || registered.Items[0].ID != "dev_l:claude" {
		t.Errorf("E09 应只含已注册 agent，实际 %+v", registered)
	}

	// since_seq 游标：limit=1 时返回最早一条并标记 has_more。
	messagesPath := "/api/v1/agents/" + url.PathEscape("dev_l:claude") + "/messages"
	var page MessageListData
	_, result = callAPI(t, ts, http.MethodGet, messagesPath+"?since_seq=0&limit=1", token, nil)
	if err := json.Unmarshal(result.Data, &page); err != nil {
		t.Fatalf("解析历史消息失败: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Seq != 1 || !page.HasMore || page.LatestSeq != 2 {
		t.Errorf("since_seq=0&limit=1 应返回 seq=1 且 has_more=true、latest_seq=2，实际 %+v", page)
	}
	_, result = callAPI(t, ts, http.MethodGet, messagesPath+"?since_seq=1&limit=1", token, nil)
	if err := json.Unmarshal(result.Data, &page); err != nil {
		t.Fatalf("解析历史消息失败: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Seq != 2 || page.HasMore {
		t.Errorf("since_seq=1&limit=1 应返回 seq=2 且 has_more=false，实际 %+v", page)
	}
	// 已到末尾再拉：空列表，不报错。
	_, result = callAPI(t, ts, http.MethodGet, messagesPath+"?since_seq=2", token, nil)
	if err := json.Unmarshal(result.Data, &page); err != nil {
		t.Fatalf("解析历史消息失败: %v", err)
	}
	if len(page.Items) != 0 || page.LatestSeq != 2 {
		t.Errorf("since_seq=2 应返回空列表且 latest_seq=2，实际 %+v", page)
	}
}

// ---------- 启动期配置/监听错误 ----------

// TestQARunRejectsInvalidListenAddr 非法监听地址必须在启动时报出可读中文错误（配置校验缺口见报告）。
func TestQARunRejectsInvalidListenAddr(t *testing.T) {
	cfg := &config.ServerConfig{
		Password: "pwd", Domain: "localhost", DeviceToken: "dev-token",
		ListenAddr: "127.0.0.1:99999", DBPath: filepath.Join(t.TempDir(), "x.db"),
		TokenTTLSeconds: 3600, HeartbeatTimeoutCycles: 3, ProtocolVersion: protocol.ProtocolVersion,
	}
	cfg.PasswordHash = config.HashHex(cfg.Password)
	cfg.DeviceTokenHash = config.HashHex(cfg.DeviceToken)

	srv, err := New(context.Background(), cfg, "0.1.0-test")
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer func() { _ = srv.store.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = srv.Run(ctx)
	if err == nil {
		t.Fatal("非法端口应启动失败，实际启动成功")
	}
	if !strings.Contains(err.Error(), "监听") {
		t.Errorf("非法端口错误应包含中文「监听」前缀，实际 %q", err.Error())
	}
}

// ---------- 停止生成（R-14 / F10） ----------

// TestQAStopKeepsPartialOutput 设备不可达时 chat.stop 必须在平台侧兜底：
// 已产出的增量保留、占位消息落为 stopped、并向浏览器下行 chat.done{stopped}。
func TestQAStopKeepsPartialOutput(t *testing.T) {
	srv, ts := newTestServer(t)
	ctx := context.Background()
	sessionID := insertFixture(t, srv.store, "dev_stop", "dev_stop:claude", protocol.AgentTypeClaude, 0)
	if _, err := srv.store.DB.ExecContext(ctx,
		`INSERT INTO messages(session_id, seq, role, content, status, created_at) VALUES(?, 1, ?, '', ?, ?)`,
		sessionID, protocol.RoleAssistant, protocol.MessageStatusStreaming, nowUTC()); err != nil {
		t.Fatalf("插入 streaming 占位失败: %v", err)
	}

	token := login(t, ts)
	client := dialClientWS(t, ts, token)
	if _, ok := readUntil(client, protocol.FrameClientReady, 3*time.Second); !ok {
		t.Fatal("未收到 client.ready")
	}

	// 设备未连接 → 平台本地兜底。
	srv.chat.Stop(ctx, protocol.ChatStopPayload{AgentID: "dev_stop:claude", SessionID: sessionID})

	frame, ok := readUntil(client, protocol.FrameChatDone, 3*time.Second)
	if !ok {
		t.Fatal("停止后应下行 chat.done")
	}
	var done protocol.ChatDonePayload
	if err := frame.DecodePayload(&done); err != nil {
		t.Fatalf("解析 chat.done 失败: %v", err)
	}
	if done.Status != protocol.DoneStatusStopped {
		t.Errorf("停止后 chat.done 状态应为 stopped，实际 %q", done.Status)
	}

	var status, content string
	if err := srv.store.DB.QueryRowContext(ctx,
		`SELECT status, content FROM messages WHERE session_id = ? AND seq = 1`, sessionID).
		Scan(&status, &content); err != nil {
		t.Fatalf("读取占位消息失败: %v", err)
	}
	if status != protocol.MessageStatusStopped {
		t.Errorf("停止后 messages.status 应为 stopped，实际 %q", status)
	}
	if content != "" {
		t.Errorf("已产出内容不应被清空，实际 %q", content)
	}
}

// TestQABadDBPathMessages 非法 SQLite 路径（父目录是一个普通文件）必须给出可读中文错误。
func TestQABadDBPathMessages(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("写入阻塞文件失败: %v", err)
	}
	_, err := OpenStore(context.Background(), filepath.Join(blocker, "agenthub.db"))
	if err == nil {
		t.Fatal("父目录是文件时应打开失败")
	}
	if !strings.Contains(err.Error(), "数据库") && !strings.Contains(err.Error(), "SQLite") {
		t.Errorf("错误信息应为可读中文（含「数据库」或「SQLite」），实际 %q", err.Error())
	}
	if _, statErr := os.Stat(filepath.Join(blocker, "agenthub.db")); statErr == nil {
		t.Error("失败时不应留下数据库文件")
	}
}
