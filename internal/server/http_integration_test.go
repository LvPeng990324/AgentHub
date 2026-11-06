package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/agenthub/agenthub/internal/config"
	"github.com/agenthub/agenthub/pkg/protocol"
)

// apiResult 保留原始 data 便于按端点解析具体结构。
type apiResult struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// newTestServer 构造一个真实 Server（真实 SQLite + 全部路由），用 httptest 承载。
func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cfg := &config.ServerConfig{
		Password:               "test-login-password",
		Domain:                 "localhost",
		ListenAddr:             "127.0.0.1:0",
		DeviceToken:            "test-device-token",
		DBPath:                 filepath.Join(t.TempDir(), "agenthub.db"),
		TokenTTLSeconds:        3600,
		HeartbeatTimeoutCycles: 3,
		LogLevel:               "error",
		ProtocolVersion:        protocol.ProtocolVersion,
	}
	cfg.PasswordHash = config.HashHex(cfg.Password)
	cfg.DeviceTokenHash = config.HashHex(cfg.DeviceToken)
	srv, err := New(context.Background(), cfg, "0.1.0-test")
	if err != nil {
		t.Fatalf("构造 Server 失败: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = srv.store.Close()
	})
	return srv, ts
}

// callAPI 发起一次 REST 调用并解析统一包络。
func callAPI(t *testing.T, ts *httptest.Server, method string, path string, token string, body any) (int, apiResult) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := ts.Client().Do(request)
	if err != nil {
		t.Fatalf("请求 %s %s 失败: %v", method, path, err)
	}
	defer response.Body.Close()
	var result apiResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("解析响应包络失败 (%s %s): %v", method, path, err)
	}
	return response.StatusCode, result
}

// login 用配置中的密码换取登录 token。
func login(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	status, result := callAPI(t, ts, http.MethodPost, "/api/v1/auth/login", "",
		LoginRequest{Password: "test-login-password"})
	if status != http.StatusOK || result.Code != protocol.CodeOK {
		t.Fatalf("登录失败: status=%d code=%d msg=%s", status, result.Code, result.Message)
	}
	var data LoginData
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatalf("解析登录数据失败: %v", err)
	}
	if data.Token == "" || data.TokenType != "Bearer" || data.ExpiresIn != 3600 {
		t.Fatalf("登录数据异常: %+v", data)
	}
	return data.Token
}

// TestRESTEndpointsAndE11 端到端走通验收要点 4/5：
// meta（公开）→ 登录 → Bearer 列表 → 注册建会话 → 造消息 → E11 取消注册 → 重新注册 → 登出。
func TestRESTEndpointsAndE11(t *testing.T) {
	srv, ts := newTestServer(t)
	ctx := context.Background()

	// E04 /meta 公开且不泄露敏感配置。
	status, result := callAPI(t, ts, http.MethodGet, "/api/v1/meta", "", nil)
	if status != http.StatusOK || result.Code != protocol.CodeOK {
		t.Fatalf("/meta 失败: status=%d code=%d", status, result.Code)
	}
	var meta MetaData
	if err := json.Unmarshal(result.Data, &meta); err != nil {
		t.Fatalf("解析 meta 失败: %v", err)
	}
	if meta.Name != "AgentHub" || meta.Domain != "localhost" || len(meta.AgentTypes) != 9 {
		t.Errorf("meta 内容异常: %+v", meta)
	}
	for _, secret := range []string{"password", "device_token", "listen_addr"} {
		if strings.Contains(string(result.Data), secret) {
			t.Errorf("/meta 泄露了敏感字段 %s", secret)
		}
	}

	// E05 /health 公开。
	if status, result = callAPI(t, ts, http.MethodGet, "/api/v1/health", "", nil); status != http.StatusOK || result.Code != protocol.CodeOK {
		t.Fatalf("/health 失败: status=%d code=%d", status, result.Code)
	}

	// 未登录访问受保护端点 → 1001 / 401。
	if status, result = callAPI(t, ts, http.MethodGet, "/api/v1/devices", "", nil); status != http.StatusUnauthorized || result.Code != protocol.CodeUnauthorized {
		t.Errorf("未登录应返回 1001，实际 status=%d code=%d", status, result.Code)
	}

	// E01 密码错误 → 1002。
	if status, result = callAPI(t, ts, http.MethodPost, "/api/v1/auth/login", "",
		LoginRequest{Password: "wrong"}); status != http.StatusUnauthorized || result.Code != protocol.CodeInvalidPassword {
		t.Errorf("密码错误应返回 1002，实际 status=%d code=%d", status, result.Code)
	}

	// E01 登录成功 → Bearer token。
	token := login(t, ts)

	// E03 /auth/me。
	if status, result = callAPI(t, ts, http.MethodGet, "/api/v1/auth/me", token, nil); status != http.StatusOK || result.Code != protocol.CodeOK {
		t.Fatalf("/auth/me 失败: status=%d code=%d", status, result.Code)
	}

	// E06 / E08 / E09 空列表正常返回（items 为非 null 空数组）。
	for _, endpoint := range []string{"/api/v1/devices", "/api/v1/agents/discovered", "/api/v1/agents"} {
		status, result = callAPI(t, ts, http.MethodGet, endpoint, token, nil)
		if status != http.StatusOK || result.Code != protocol.CodeOK {
			t.Fatalf("%s 失败: status=%d code=%d", endpoint, status, result.Code)
		}
		if !strings.Contains(string(result.Data), `"items":[]`) {
			t.Errorf("%s 应返回空数组 items，实际 %s", endpoint, result.Data)
		}
	}

	// 造一条 device → agent 数据（模拟适配器 F01 全量快照）。
	if err := srv.deviceRepo.Upsert(ctx, DeviceUpsert{
		ID: "dev_int", Name: "集成测试设备", OS: "windows", Arch: "amd64",
		AdapterVersion: "0.1.0", ProtocolVersion: protocol.ProtocolVersion,
	}); err != nil {
		t.Fatalf("写入设备失败: %v", err)
	}
	if err := srv.agentRepo.UpsertSnapshot(ctx, "dev_int", []protocol.AgentSnapshot{{
		Type: protocol.AgentTypeClaude, DisplayName: "Claude Code",
		Version: "1.0.0", ExecPath: "/bin/claude", Status: protocol.AgentStatusAvailable,
	}}); err != nil {
		t.Fatalf("写入 agent 快照失败: %v", err)
	}
	agentID := "dev_int:claude"
	encodedAgentID := url.PathEscape(agentID)

	// 备注保留校验前置。
	if _, err := srv.store.DB.ExecContext(ctx,
		`UPDATE agents SET remark = '主力' WHERE id = ?`, agentID); err != nil {
		t.Fatalf("设置备注失败: %v", err)
	}

	// E08 已发现列表含未注册 agent。
	if status, result = callAPI(t, ts, http.MethodGet, "/api/v1/agents/discovered", token, nil); result.Code != protocol.CodeOK {
		t.Fatalf("discovered 失败: %d", result.Code)
	}
	var discovered AgentListData
	if err := json.Unmarshal(result.Data, &discovered); err != nil {
		t.Fatalf("解析 discovered 失败: %v", err)
	}
	if discovered.Total != 1 || len(discovered.Items) != 1 || discovered.Items[0].Registered {
		t.Errorf("discovered 应含 1 个未注册 agent，实际 %+v", discovered)
	}

	// E10 注册 → 必须返回 session_id。
	status, result = callAPI(t, ts, http.MethodPost, "/api/v1/agents/"+encodedAgentID+"/register", token, nil)
	if status != http.StatusOK || result.Code != protocol.CodeOK {
		t.Fatalf("注册失败: status=%d code=%d msg=%s", status, result.Code, result.Message)
	}
	var registeredAgent AgentDTO
	if err := json.Unmarshal(result.Data, &registeredAgent); err != nil {
		t.Fatalf("解析注册结果失败: %v", err)
	}
	if !registeredAgent.Registered || registeredAgent.SessionID == nil || *registeredAgent.SessionID == "" {
		t.Fatalf("注册结果应含 session_id，实际 %+v", registeredAgent)
	}
	sessionID := *registeredAgent.SessionID

	// 造一轮对话消息（user + assistant 占位，再落库完整文本）。
	_, assistantSeq, err := srv.messageRepo.AppendTurn(ctx, sessionID, "帮我看下构建报错")
	if err != nil {
		t.Fatalf("AppendTurn 失败: %v", err)
	}
	if err := srv.messageRepo.UpdateMessageContent(ctx, sessionID, assistantSeq,
		"我先检查一下……", protocol.MessageStatusComplete); err != nil {
		t.Fatalf("落库助手消息失败: %v", err)
	}

	// E15 session / E14 messages。
	status, result = callAPI(t, ts, http.MethodGet, "/api/v1/agents/"+encodedAgentID+"/session", token, nil)
	if result.Code != protocol.CodeOK {
		t.Fatalf("session 失败: %d", result.Code)
	}
	var sessionData SessionData
	if err := json.Unmarshal(result.Data, &sessionData); err != nil {
		t.Fatalf("解析 session 失败: %v", err)
	}
	if sessionData.SessionID == nil || sessionData.MessageCount != 2 {
		t.Errorf("session 数据异常: %+v", sessionData)
	}
	status, result = callAPI(t, ts, http.MethodGet,
		"/api/v1/agents/"+encodedAgentID+"/messages?since_seq=0&limit=200", token, nil)
	if result.Code != protocol.CodeOK {
		t.Fatalf("messages 失败: %d", result.Code)
	}
	var messageList MessageListData
	if err := json.Unmarshal(result.Data, &messageList); err != nil {
		t.Fatalf("解析 messages 失败: %v", err)
	}
	if len(messageList.Items) != 2 || messageList.LatestSeq != 2 || messageList.HasMore {
		t.Errorf("messages 数据异常: %+v", messageList)
	}

	// ★E11 取消注册。
	status, result = callAPI(t, ts, http.MethodDelete, "/api/v1/agents/"+encodedAgentID+"/register", token, nil)
	if status != http.StatusOK || result.Code != protocol.CodeOK {
		t.Fatalf("取消注册失败: status=%d code=%d msg=%s", status, result.Code, result.Message)
	}
	var unregisterData UnregisterData
	if err := json.Unmarshal(result.Data, &unregisterData); err != nil {
		t.Fatalf("解析取消注册结果失败: %v", err)
	}
	if unregisterData.AgentID != agentID || unregisterData.Registered || !unregisterData.MessagesCleared {
		t.Errorf("取消注册响应异常: %+v", unregisterData)
	}

	// 库级断言：registered=0、sessions/messages 清空、devices/agents/remark 保留。
	var registeredInt, onlineInt int
	var remark string
	if err := srv.store.DB.QueryRowContext(ctx,
		`SELECT registered, online, remark FROM agents WHERE id = ?`, agentID).
		Scan(&registeredInt, &onlineInt, &remark); err != nil {
		t.Fatalf("读取 agent 失败: %v", err)
	}
	if registeredInt != 0 || remark != "主力" {
		t.Errorf("取消注册后 registered/remark 异常: registered=%d remark=%q", registeredInt, remark)
	}
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM devices`); got != 1 {
		t.Errorf("devices 行应保留，实际 %d", got)
	}
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM agents`); got != 1 {
		t.Errorf("agents 行应保留，实际 %d", got)
	}
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM sessions`); got != 0 {
		t.Errorf("sessions 应清空，实际 %d", got)
	}
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM messages`); got != 0 {
		t.Errorf("messages 应清空，实际 %d", got)
	}

	// 取消注册后取会话 → 3002；重新注册 → 新会话且历史为空。
	if status, result = callAPI(t, ts, http.MethodGet, "/api/v1/agents/"+encodedAgentID+"/session", token, nil); result.Code != protocol.CodeAgentNotRegistered {
		t.Errorf("未注册取会话应返回 3002，实际 %d", result.Code)
	}
	status, result = callAPI(t, ts, http.MethodPost, "/api/v1/agents/"+encodedAgentID+"/register", token, nil)
	if result.Code != protocol.CodeOK {
		t.Fatalf("重新注册失败: %d", result.Code)
	}
	var reRegistered AgentDTO
	if err := json.Unmarshal(result.Data, &reRegistered); err != nil {
		t.Fatalf("解析重新注册结果失败: %v", err)
	}
	if reRegistered.SessionID == nil || *reRegistered.SessionID == sessionID {
		t.Errorf("重新注册应新建会话，实际 %+v", reRegistered.SessionID)
	}
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM messages`); got != 0 {
		t.Errorf("重新注册后历史消息应为空，实际 %d", got)
	}

	// E02 登出后 token 即时失效。
	if status, result = callAPI(t, ts, http.MethodPost, "/api/v1/auth/logout", token, nil); status != http.StatusOK || result.Code != protocol.CodeOK {
		t.Fatalf("登出失败: status=%d code=%d", status, result.Code)
	}
	if status, result = callAPI(t, ts, http.MethodGet, "/api/v1/auth/me", token, nil); status != http.StatusUnauthorized || result.Code != protocol.CodeUnauthorized {
		t.Errorf("登出后 token 应失效，实际 status=%d code=%d", status, result.Code)
	}
}

// TestWSHandshakeAuth 校验两条 WS 通道 Upgrade 之前的鉴权与状态码。
func TestWSHandshakeAuth(t *testing.T) {
	_, ts := newTestServer(t)

	// ws/client 只接受登录 token；设备令牌不能登录。
	badTokens := []string{"", "test-device-token", "invalid"}
	for _, token := range badTokens {
		status, result := callAPI(t, ts, http.MethodGet,
			"/api/v1/ws/client?token="+token, "", nil)
		if status != http.StatusUnauthorized {
			t.Errorf("ws/client token=%q 期望 401，实际 %d (code=%d)", maskToken(token), status, result.Code)
		}
	}
	// ws/adapter 设备令牌错误 → 5001 / 401；协议版本不匹配 → 5002 / 426；缺 device_id → 1004 / 400。
	if status, result := callAPI(t, ts, http.MethodGet,
		"/api/v1/ws/adapter?token=wrong&device_id=dev_x", "", nil); status != http.StatusUnauthorized || result.Code != protocol.CodeAdapterUnauthorized {
		t.Errorf("设备令牌无效应返回 5001/401，实际 %d/%d", status, result.Code)
	}
	if status, result := callAPI(t, ts, http.MethodGet,
		"/api/v1/ws/adapter?token=test-device-token&device_id=dev_x&protocol_version=2", "", nil); status != 426 || result.Code != protocol.CodeProtocolVersionMismatch {
		t.Errorf("协议版本不匹配应返回 5002/426，实际 %d/%d", status, result.Code)
	}
	if status, result := callAPI(t, ts, http.MethodGet,
		"/api/v1/ws/adapter?token=test-device-token", "", nil); status != http.StatusBadRequest || result.Code != protocol.CodeBadRequest {
		t.Errorf("缺 device_id 应返回 1004/400，实际 %d/%d", status, result.Code)
	}
}

// TestWSClientReadyFrame 校验登录态握手成功后立即下行 F14 client.ready（seq=1）。
func TestWSClientReadyFrame(t *testing.T) {
	_, ts := newTestServer(t)
	token := login(t, ts)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws/client?token=" + token
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws/client 握手失败: %v", err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("设置读超时失败: %v", err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("读取首帧失败: %v", err)
	}
	var frame protocol.Message
	if err := frame.Decode(data); err != nil {
		t.Fatalf("解析首帧失败: %v", err)
	}
	if frame.Type != protocol.FrameClientReady || frame.Seq != 1 {
		t.Errorf("首帧应为 client.ready seq=1，实际 type=%s seq=%d", frame.Type, frame.Seq)
	}
	var ready protocol.ClientReadyPayload
	if err := frame.DecodePayload(&ready); err != nil {
		t.Fatalf("解析 client.ready payload 失败: %v", err)
	}
	if ready.ProtocolVersion != protocol.ProtocolVersion || ready.ServerTime == "" {
		t.Errorf("client.ready payload 异常: %+v", ready)
	}
}

// TestWSAdapterRegisterFlow 校验 F01 全量快照 → F02 register_ack 与库侧 UpsertSnapshot 落库。
func TestWSAdapterRegisterFlow(t *testing.T) {
	srv, ts := newTestServer(t)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/api/v1/ws/adapter?token=test-device-token&device_id=dev_ws&protocol_version=1"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws/adapter 握手失败: %v", err)
	}
	defer conn.Close()

	register := protocol.DeviceRegisterPayload{
		DeviceID: "dev_ws", Name: "WS 测试设备", OS: "windows", Arch: "amd64",
		AdapterVersion: "0.1.0", ProtocolVersion: protocol.ProtocolVersion,
		Agents: []protocol.AgentSnapshot{{
			Type: protocol.AgentTypeClaude, DisplayName: "Claude Code",
			Version: "1.0.0", ExecPath: "/bin/claude", Status: protocol.AgentStatusAvailable,
		}},
	}
	frame, err := protocol.NewMessage(protocol.FrameDeviceRegister, 1, register)
	if err != nil {
		t.Fatalf("构造 F01 失败: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, frame.Encode()); err != nil {
		t.Fatalf("发送 F01 失败: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("设置读超时失败: %v", err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("读取 F02 失败: %v", err)
	}
	var ackFrame protocol.Message
	if err := ackFrame.Decode(data); err != nil {
		t.Fatalf("解析 F02 失败: %v", err)
	}
	if ackFrame.Type != protocol.FrameDeviceRegisterAck {
		t.Errorf("期望 device.register_ack，实际 %s", ackFrame.Type)
	}
	var ack protocol.DeviceRegisterAckPayload
	if err := ackFrame.DecodePayload(&ack); err != nil {
		t.Fatalf("解析 F02 payload 失败: %v", err)
	}
	if !ack.Accepted || ack.DeviceID != "dev_ws" ||
		ack.HeartbeatIntervalS != protocol.HeartbeatIntervalSeconds ||
		ack.ProtocolVersion != protocol.ProtocolVersion {
		t.Errorf("F02 内容异常: %+v", ack)
	}
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM devices WHERE id = ?`, "dev_ws"); got != 1 {
		t.Errorf("F01 应落库设备，实际 %d", got)
	}
	if got := countRows(t, srv.store, `SELECT COUNT(*) FROM agents WHERE id = ?`, "dev_ws:claude"); got != 1 {
		t.Errorf("F01 应落库 agent，实际 %d", got)
	}
}
