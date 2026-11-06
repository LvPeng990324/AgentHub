// Command smoke-helper 是 scripts/smoke.sh 的 Go 辅助程序，三个模式：
//
//	genconfig  生成临时 server.json / adapter.json（避免依赖 jq / python）
//	fake-cli   扮演一个 stream-json 假 CLI（被 adapter 的 agent_paths 指向）
//	ws         浏览器侧 WS 客户端：登录 → 两轮 chat.send → 断言 ack/chunk/done/落库/续接
//
// 帧名与错误码一律引用 pkg/protocol，不在本文件硬编码。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/agenthub/agenthub/pkg/protocol"
)

const (
	markerEnv      = "AGENTHUB_SMOKE_MARKER"
	sessionEnv     = "AGENTHUB_SMOKE_SESSION_ID"
	defaultSession = "ses-fake-smoke"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	// 当可执行文件名为 *fake-cli* 时，无需模式参数，直接扮演假 CLI。
	if strings.Contains(strings.ToLower(filepath.Base(os.Args[0])), "fake-cli") {
		return fakeCLI()
	}
	mode := ""
	if len(args) > 0 {
		mode = args[0]
	}
	switch mode {
	case "genconfig":
		return genConfig(args[1:])
	case "ws":
		return runWS(args[1:])
	case "fake-cli":
		return fakeCLI()
	default:
		return fmt.Errorf("未知模式 %q（可用: genconfig | fake-cli | ws）", mode)
	}
}

// ---------------------------------------------------------------------------
// genconfig
// ---------------------------------------------------------------------------

func genConfig(args []string) error {
	flags := flag.NewFlagSet("genconfig", flag.ExitOnError)
	tmp := flags.String("tmp", "", "临时目录")
	port := flags.Int("port", 0, "平台监听端口")
	fakeCLI := flags.String("fake", "", "假 CLI 可执行文件绝对路径")
	password := flags.String("password", "", "测试登录密码")
	token := flags.String("token", "", "测试设备令牌")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *tmp == "" || *port == 0 || *fakeCLI == "" || *password == "" || *token == "" {
		return errors.New("genconfig 缺少必需参数")
	}

	server := map[string]any{
		"server": map[string]any{
			"password":                 *password,
			"domain":                   "localhost",
			"listen_addr":              fmt.Sprintf("127.0.0.1:%d", *port),
			"device_token":             *token,
			"db_path":                  filepath.Join(*tmp, "smoke.db"),
			"token_ttl_seconds":        3600,
			"heartbeat_timeout_cycles": 3,
			"log_level":                "error",
			"protocol_version":         protocol.ProtocolVersion,
		},
	}
	adapter := map[string]any{
		"adapter": map[string]any{
			"platform_ws_url":            fmt.Sprintf("ws://127.0.0.1:%d/api/v1/ws/adapter", *port),
			"device_token":               *token,
			"device_name":                "冒烟测试设备",
			"discover_period_seconds":    120,
			"heartbeat_interval_seconds": 1,
			"device_id_path":             filepath.Join(*tmp, "device_id"),
			"agent_paths":                map[string]string{protocol.AgentTypeClaude: *fakeCLI},
			"log_level":                  "error",
		},
	}
	if err := writeJSON(filepath.Join(*tmp, "server.json"), server); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*tmp, "adapter.json"), adapter); err != nil {
		return err
	}
	fmt.Println("  PASS: 已生成临时 server.json / adapter.json")
	return nil
}

func writeJSON(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}

// ---------------------------------------------------------------------------
// fake-cli（S 族 stream-json）
// ---------------------------------------------------------------------------

func fakeCLI() error {
	args := os.Args[1:]
	for _, arg := range args {
		if arg == "--version" {
			fmt.Println("fake-cli 1.0.0")
			return nil
		}
	}
	// prompt 永远走 stdin；等平台写端关闭。
	_, _ = io.ReadAll(os.Stdin)

	resumeID := ""
	for index, arg := range args {
		if arg == "--resume" && index+1 < len(args) {
			resumeID = args[index+1]
			break
		}
	}
	if resumeID != "" {
		if marker := os.Getenv(markerEnv); marker != "" {
			if err := os.WriteFile(marker, []byte(resumeID), 0o600); err != nil {
				return fmt.Errorf("写入续接标记失败: %w", err)
			}
		}
		fmt.Println(`{"type":"content_block_delta","delta":{"text":"[续接]"}}`)
	} else {
		fmt.Println(`{"type":"content_block_delta","delta":{"text":"你好，"}}`)
		fmt.Println(`{"type":"content_block_delta","delta":{"text":"这是冒烟假 CLI 的回复"}}`)
	}
	sessionID := os.Getenv(sessionEnv)
	if sessionID == "" {
		sessionID = defaultSession
	}
	// ★S 族 fail-closed：必须输出 result 事件，否则解析器判 failed。
	fmt.Printf(`{"type":"result","subtype":"success","session_id":%q}`+"\n", sessionID)
	return nil
}

// ---------------------------------------------------------------------------
// ws：浏览器侧全链路断言
// ---------------------------------------------------------------------------

type frame struct {
	Type    string          `json:"type"`
	Seq     int64           `json:"seq"`
	Payload json.RawMessage `json:"payload"`
}

type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func apiCall(base string, method string, path string, token string, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var result envelope
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if result.Code != protocol.CodeOK {
		return nil, fmt.Errorf("业务错误 %d: %s", result.Code, result.Message)
	}
	return result.Data, nil
}

func login(base string, password string) (string, error) {
	data, err := apiCall(base, http.MethodPost, "/api/v1/auth/login", "",
		map[string]string{"password": password})
	if err != nil {
		return "", err
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Token == "" {
		return "", errors.New("登录未返回 token")
	}
	return payload.Token, nil
}

func readFrame(connection *websocket.Conn) (frame, error) {
	if err := connection.SetReadDeadline(time.Now().Add(35 * time.Second)); err != nil {
		return frame{}, err
	}
	_, data, err := connection.ReadMessage()
	if err != nil {
		return frame{}, err
	}
	var result frame
	if err := json.Unmarshal(data, &result); err != nil {
		return frame{}, err
	}
	return result, nil
}

func sendChat(connection *websocket.Conn, seq int64, agentID string, content string) error {
	message, err := protocol.NewMessage(protocol.FrameChatSend, seq,
		map[string]string{"agent_id": agentID, "content": content})
	if err != nil {
		return err
	}
	encoded, err := message.Marshal()
	if err != nil {
		return err
	}
	return connection.WriteMessage(websocket.TextMessage, encoded)
}

type turnResult struct {
	ack    protocol.ChatSendAckPayload
	chunks []protocol.ChatChunkPayload
	done   protocol.ChatDonePayload
}

func waitTurn(connection *websocket.Conn) (turnResult, error) {
	var result turnResult
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		incoming, err := readFrame(connection)
		if err != nil {
			return result, err
		}
		switch incoming.Type {
		case protocol.FrameChatSendAck:
			if err := json.Unmarshal(incoming.Payload, &result.ack); err != nil {
				return result, err
			}
		case protocol.FrameChatChunk:
			var chunk protocol.ChatChunkPayload
			if err := json.Unmarshal(incoming.Payload, &chunk); err != nil {
				return result, err
			}
			result.chunks = append(result.chunks, chunk)
		case protocol.FrameChatDone:
			if err := json.Unmarshal(incoming.Payload, &result.done); err != nil {
				return result, err
			}
			return result, nil
		case protocol.FrameChatError:
			var failure protocol.ChatErrorPayload
			_ = json.Unmarshal(incoming.Payload, &failure)
			return result, fmt.Errorf("收到 chat.error %d: %s", failure.Code, failure.Message)
		default:
			// client.ready / agent.status / device.status 忽略。
		}
	}
	return result, errors.New("等待 chat.done 超时")
}

func check(condition bool, message string) error {
	if !condition {
		return errors.New(message)
	}
	fmt.Println("  PASS:", message)
	return nil
}

func runWS(args []string) error {
	flags := flag.NewFlagSet("ws", flag.ExitOnError)
	base := flags.String("base", "", "平台 HTTP 基址，如 http://127.0.0.1:18082")
	password := flags.String("password", "", "测试登录密码")
	agentID := flags.String("agent", "", "agent_id（空则自动从 /agents/discovered 取）")
	marker := flags.String("marker", "", "续接标记文件")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *base == "" || *password == "" {
		return errors.New("ws 缺少 -base / -password")
	}

	token, err := login(*base, *password)
	if err != nil {
		return err
	}
	if *agentID == "" {
		data, err := apiCall(*base, http.MethodGet, "/api/v1/agents/discovered", token, nil)
		if err != nil {
			return err
		}
		var listing struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if err := json.Unmarshal(data, &listing); err != nil || len(listing.Items) == 0 {
			return errors.New("未发现任何 agent")
		}
		*agentID = listing.Items[0].ID
	}
	encodedID := url.PathEscape(*agentID)

	wsURL := "ws" + strings.TrimPrefix(*base, "http") +
		"/api/v1/ws/client?token=" + url.QueryEscape(token)
	connection, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("WS 握手失败: %w", err)
	}
	defer connection.Close()

	first, err := readFrame(connection)
	if err != nil {
		return err
	}
	if err := check(first.Type == protocol.FrameClientReady,
		"WS 建连首帧为 client.ready"); err != nil {
		return err
	}

	// ---- 第一轮：发送 → ack → 流式 chunk → done ----
	if err := sendChat(connection, 1, *agentID, "第一轮消息"); err != nil {
		return err
	}
	firstTurn, err := waitTurn(connection)
	if err != nil {
		return err
	}
	if err := check(firstTurn.ack.AssistantSeq > 0 && firstTurn.ack.UserSeq > 0,
		"收到 chat.send_ack（含平台分配的 user_seq / assistant_seq）"); err != nil {
		return err
	}
	if err := check(len(firstTurn.chunks) >= 1,
		fmt.Sprintf("收到 %d 个 chat.chunk 流式增量", len(firstTurn.chunks))); err != nil {
		return err
	}
	for index, chunk := range firstTurn.chunks {
		if err := check(chunk.ChunkIndex == index,
			fmt.Sprintf("chunk_index 单调递增（%d）", chunk.ChunkIndex)); err != nil {
			return err
		}
	}
	if err := check(firstTurn.done.Status == protocol.DoneStatusCompleted,
		"收到 chat.done{completed}"); err != nil {
		return err
	}
	if err := check(firstTurn.done.ResumeID != "",
		"chat.done 回带 resume_id"); err != nil {
		return err
	}

	messagesData, err := apiCall(*base, http.MethodGet,
		"/api/v1/agents/"+encodedID+"/messages?since_seq=0&limit=200", token, nil)
	if err != nil {
		return err
	}
	var messageList struct {
		Items []struct {
			Seq     int64  `json:"seq"`
			Role    string `json:"role"`
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"items"`
		LatestSeq int64 `json:"latest_seq"`
	}
	if err := json.Unmarshal(messagesData, &messageList); err != nil {
		return err
	}
	if err := check(len(messageList.Items) >= 2, "历史消息落库（user + assistant）"); err != nil {
		return err
	}
	monotonic := true
	hasUser := false
	hasCompleteAssistant := false
	for index, item := range messageList.Items {
		if index > 0 && item.Seq <= messageList.Items[index-1].Seq {
			monotonic = false
		}
		if item.Role == protocol.RoleUser && item.Content != "" {
			hasUser = true
		}
		if item.Role == protocol.RoleAssistant &&
			item.Status == protocol.MessageStatusComplete && item.Content != "" {
			hasCompleteAssistant = true
		}
	}
	if err := check(monotonic, "会话级 seq 单调不重复"); err != nil {
		return err
	}
	if err := check(hasUser, "用户消息已落库"); err != nil {
		return err
	}
	if err := check(hasCompleteAssistant, "助手完整文本已落库且 status=complete"); err != nil {
		return err
	}

	// ---- 第二轮：续接（平台下发 resume_id → 适配器追加 --resume） ----
	if err := sendChat(connection, 2, *agentID, "第二轮消息"); err != nil {
		return err
	}
	secondTurn, err := waitTurn(connection)
	if err != nil {
		return err
	}
	if err := check(secondTurn.done.Status == protocol.DoneStatusCompleted,
		"第二轮 chat.done{completed}"); err != nil {
		return err
	}
	if err := check(secondTurn.ack.UserSeq > firstTurn.ack.AssistantSeq,
		"第二轮会话级 seq 继续递增"); err != nil {
		return err
	}
	sessionData, err := apiCall(*base, http.MethodGet,
		"/api/v1/agents/"+encodedID+"/session", token, nil)
	if err != nil {
		return err
	}
	var session struct {
		ResumeID     string `json:"resume_id"`
		MessageCount int64  `json:"message_count"`
	}
	if err := json.Unmarshal(sessionData, &session); err != nil {
		return err
	}
	if err := check(session.ResumeID != "", "平台 sessions.resume_id 已写入"); err != nil {
		return err
	}
	if err := check(session.MessageCount >= 4, "两轮共 4 条消息已落库"); err != nil {
		return err
	}
	if *marker != "" {
		content, err := os.ReadFile(*marker)
		if err != nil {
			return fmt.Errorf("假 CLI 未收到 --resume（标记文件不存在）: %w", err)
		}
		if err := check(strings.TrimSpace(string(content)) == session.ResumeID,
			"假 CLI 收到 --resume <平台 resume_id>，会话续接生效"); err != nil {
			return err
		}
	}
	fmt.Println("  PASS: 发消息 → 流式回复 → 落库 → 续接 全链路")
	return nil
}
