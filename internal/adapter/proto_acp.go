package adapter

// A 族解析器（§8.8）：hermes / kimi 的 JSON-RPC 2.0 over NDJSON。
// 本文件同时提供 ACP 与 app-server 共用的 rpcClient 与 runRPCTurn，避免两族重复实现。
//
// 流程：initialize → session/new|session/resume → session/prompt；
// session/update(agent_message_chunk) → MsgText；session/request_permission 自动批准；
// 恢复被拒 → ClassifyResumeFailure → 降级新会话 + Result.ResumeRejected=true。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agenthub/agenthub/pkg/protocol"
)

type rpcFrame map[string]any

// rpcClient 是一个最小的 JSON-RPC over NDJSON 客户端：pending-map 关联 id，
// 通知与服务端请求分别投递到两个 channel（供主循环处理权限自动应答）。
type rpcClient struct {
	stdin          io.Writer
	scanner        *bufio.Scanner
	writeMu        sync.Mutex
	nextID         atomic.Int64
	pendingMu      sync.Mutex
	pending        map[int64]chan rpcFrame
	notifications  chan rpcFrame
	serverRequests chan rpcFrame
	done           chan struct{}
	closeOnce      sync.Once
}

func newRPCClient(stdin io.Writer, stdout io.Reader) *rpcClient {
	return &rpcClient{
		stdin:          stdin,
		scanner:        newAgentStreamScanner(stdout),
		pending:        make(map[int64]chan rpcFrame),
		notifications:  make(chan rpcFrame, 128),
		serverRequests: make(chan rpcFrame, 16),
		done:           make(chan struct{}),
	}
}

func (c *rpcClient) close() {
	c.closeOnce.Do(func() { close(c.done) })
}

// readLoop 把响应 / 通知 / 服务端请求分流；stdout EOF 时关闭通道。
func (c *rpcClient) readLoop() {
	defer c.close()
	for c.scanner.Scan() {
		line := trimTrailingCR(c.scanner.Text())
		if line == "" {
			continue
		}
		var frame rpcFrame
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			slog.Debug("RPC 忽略非 JSON 行", "bytes", len(line))
			continue
		}
		_, hasID := frame["id"]
		_, hasMethod := frame["method"]
		switch {
		case hasID && !hasMethod:
			id := rpcID(frame["id"])
			c.pendingMu.Lock()
			channel := c.pending[id]
			delete(c.pending, id)
			c.pendingMu.Unlock()
			if channel != nil {
				select {
				case channel <- frame:
				default:
				}
			}
		case hasID && hasMethod:
			select {
			case c.serverRequests <- frame:
			case <-c.done:
				return
			}
		case hasMethod:
			select {
			case c.notifications <- frame:
			case <-c.done:
				return
			}
		}
	}
}

func rpcID(raw any) int64 {
	switch value := raw.(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	case json.Number:
		parsed, _ := value.Int64()
		return parsed
	case string:
		var parsed int64
		if _, err := fmt.Sscanf(value, "%d", &parsed); err == nil {
			return parsed
		}
	}
	return 0
}

func (c *rpcClient) write(frame rpcFrame) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return errors.New("RPC 通道已关闭")
	default:
	}
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

func (c *rpcClient) request(
	ctx context.Context,
	method string,
	params any,
	timeout time.Duration,
) (rpcFrame, error) {
	if timeout <= 0 {
		timeout = defaultRPCTimeout
	}
	id := c.nextID.Add(1)
	channel := make(chan rpcFrame, 1)
	c.pendingMu.Lock()
	c.pending[id] = channel
	c.pendingMu.Unlock()
	frame := rpcFrame{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		frame["params"] = params
	}
	if err := c.write(frame); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case response := <-channel:
		if errObject, ok := response["error"]; ok && errObject != nil {
			return nil, fmt.Errorf("RPC %s 失败: %s", method, describeRPCError(errObject))
		}
		return response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("RPC %s 超时", method)
	case <-c.done:
		return nil, errors.New("RPC 通道已关闭")
	}
}

func (c *rpcClient) notify(method string, params any) error {
	return c.write(rpcFrame{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *rpcClient) respond(id any, result any) error {
	return c.write(rpcFrame{"jsonrpc": "2.0", "id": id, "result": result})
}

func describeRPCError(raw any) string {
	if detail, ok := raw.(map[string]any); ok {
		if message := stringFromMap(detail, "message", "error"); message != "" {
			return message
		}
	}
	return fmt.Sprint(raw)
}

// rpcTurnConfig 参数化 ACP 与 app-server 的方法名/提取器，共用一条编排逻辑。
type rpcTurnConfig struct {
	newMethod          string
	promptMethod       string
	initializedMethod  string
	handshakeTimeout   time.Duration
	promptParams       func(sessionID string, prompt string) map[string]any
	extractText        func(rpcFrame) string
	isDoneNotification func(rpcFrame) bool
	// requiresMCPServers 表示该族的 session 建立请求要求 mcpServers 字段（ACP 必填）。
	// kimi 在缺失时返回 {"code":-32602,"message":"Invalid params",
	// "data":{"mcpServers":{"_errors":["Required value is missing"]}}}。
	requiresMCPServers bool
}

// buildSessionParams 统一构造 session/new | session/resume 的 params：
//   - 复制 plan.Params（避免污染共享 map / 降级分支复用同一份）；
//   - cwd 非空才写，绝不下发 "cwd": ""（kimi 会拒绝 workspace root）；
//   - ACP 族补 mcpServers: []（必填字段）。
func buildSessionParams(planParams map[string]any, cwd string, withMCPServers bool) map[string]any {
	params := make(map[string]any, len(planParams)+2)
	for key, value := range planParams {
		params[key] = value
	}
	if cwd != "" {
		params["cwd"] = cwd
	} else {
		delete(params, "cwd")
	}
	if withMCPServers {
		if _, exists := params["mcpServers"]; !exists {
			params["mcpServers"] = []any{}
		}
	}
	return params
}

type promptOutcome struct {
	err error
}

// runRPCTurn 是 A/AS 两族共用的完整 turn 编排。
func runRPCTurn(
	ctx context.Context,
	process *Process,
	descriptor Descriptor,
	prompt string,
	plan ResumePlan,
	opts ExecOptions,
	out chan<- Message,
	cfg rpcTurnConfig,
) Result {
	startedAt := process.StartedAt()
	rpc := newRPCClient(process.Stdin, process.Stdout)
	defer rpc.close()
	go rpc.readLoop()

	handshake := opts.HandshakeTimeout
	if handshake <= 0 {
		handshake = cfg.handshakeTimeout
	}
	if handshake <= 0 {
		handshake = 30 * time.Second
	}

	initCtx, cancelInit := context.WithTimeout(ctx, handshake)
	defer cancelInit()
	if _, err := rpc.request(initCtx, rpcInitialize,
		map[string]any{"protocolVersion": protocol.ProtocolVersion, "clientCapabilities": map[string]any{}},
		handshake); err != nil {
		return rpcFailure("初始化握手失败", err, startedAt)
	}
	if cfg.initializedMethod != "" {
		_ = rpc.notify(cfg.initializedMethod, map[string]any{})
	}

	method := plan.Method
	if method == "" {
		method = cfg.newMethod
	}
	params := buildSessionParams(plan.Params, opts.Cwd, cfg.requiresMCPServers)

	resumeRejected := false
	response, err := rpc.request(ctx, method, params, handshake)
	if err != nil && ClassifyResumeFailure(err, descriptor) {
		// 恢复被拒 → 降级新会话，不无限重试死 id（R-18）。
		resumeRejected = true
		slog.Warn("会话恢复被拒，降级为新会话", "type", descriptor.Type)
		newPlan := NewSessionPlan(descriptor)
		fallbackParams := buildSessionParams(newPlan.Params, opts.Cwd, cfg.requiresMCPServers)
		response, err = rpc.request(ctx, cfg.newMethod, fallbackParams, handshake)
	}
	if err != nil {
		return rpcFailure("会话建立失败", err, startedAt)
	}
	sessionID := sessionIDFromMap(response)
	if sessionID == "" {
		// session/resume 的响应可能不含会话 id（kimi 只回 result:{}），
		// 此时必须沿用请求里携带的 id，否则后续 session/prompt 会报 "Unknown sessionId: "。
		if existing, ok := params["sessionId"].(string); ok && existing != "" {
			sessionID = existing
		} else if existing, ok := params["threadId"].(string); ok && existing != "" {
			sessionID = existing
		}
	}

	promptDone := make(chan promptOutcome, 1)
	go func() {
		_, requestErr := rpc.request(ctx, cfg.promptMethod,
			cfg.promptParams(sessionID, prompt), defaultRPCTimeout)
		promptDone <- promptOutcome{err: requestErr}
	}()

	doneSignal := make(chan struct{}, 1)
	for {
		select {
		case <-ctx.Done():
			return Result{
				Status: statusForContextErr(ctx.Err()), ResumeRejected: resumeRejected,
				DurationMs: time.Since(startedAt).Milliseconds(),
			}
		case note, ok := <-rpc.notifications:
			if !ok {
				continue
			}
			if text := cfg.extractText(note); text != "" {
				out <- Message{Type: MsgText, Content: text}
			}
			if cfg.isDoneNotification != nil && cfg.isDoneNotification(note) {
				select {
				case doneSignal <- struct{}{}:
				default:
				}
			}
		case request := <-rpc.serverRequests:
			// ★session/request_permission 必须自动批准，否则每次文件写入都会阻塞。
			_ = rpc.respond(request["id"], map[string]any{
				"outcome": map[string]any{"outcome": "selected", "optionId": "allow"},
			})
		case outcome := <-promptDone:
			drainNotifications(rpc.notifications, cfg.extractText, out)
			if outcome.err != nil {
				// 新会话在 prompt 前失败：不上报 session id。
				status := ResultFailed
				if errors.Is(outcome.err, context.DeadlineExceeded) {
					status = ResultTimeout
				}
				return Result{
					Status: status, Error: "发送 prompt 失败: " + outcome.err.Error(),
					ResumeRejected: resumeRejected, DurationMs: time.Since(startedAt).Milliseconds(),
				}
			}
			return Result{
				Status: ResultCompleted, SessionID: sessionID,
				ResumeRejected: resumeRejected, DurationMs: time.Since(startedAt).Milliseconds(),
			}
		case <-doneSignal:
			drainNotifications(rpc.notifications, cfg.extractText, out)
			return Result{
				Status: ResultCompleted, SessionID: sessionID,
				ResumeRejected: resumeRejected, DurationMs: time.Since(startedAt).Milliseconds(),
			}
		case <-rpc.done:
			return rpcFailure("RPC 通道意外关闭", errors.New("stdout 已关闭"), startedAt)
		}
	}
}

// drainNotifications 在收尾前把 150ms 内到达的增量全部投递，避免丢尾帧。
func drainNotifications(
	notifications <-chan rpcFrame,
	extract func(rpcFrame) string,
	out chan<- Message,
) {
	deadline := time.NewTimer(150 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case note := <-notifications:
			if text := extract(note); text != "" {
				out <- Message{Type: MsgText, Content: text}
			}
		case <-deadline.C:
			return
		}
	}
}

func rpcFailure(prefix string, err error, startedAt time.Time) Result {
	status := ResultFailed
	if errors.Is(err, context.DeadlineExceeded) {
		// 握手 / prompt 阶段的超时同样映射 5005，而不是误报「CLI 启动失败」。
		status = ResultTimeout
	}
	return Result{
		Status:     status,
		Error:      prefix + ": " + err.Error(),
		DurationMs: time.Since(startedAt).Milliseconds(),
	}
}

// ACPParser 解析 hermes / kimi。
type ACPParser struct{}

// NewACPParser 构造 A 族解析器。
func NewACPParser() *ACPParser {
	return &ACPParser{}
}

// Parse 启动 ACP turn。
func (p *ACPParser) Parse(
	ctx context.Context,
	process *Process,
	descriptor Descriptor,
	prompt string,
	plan ResumePlan,
	opts ExecOptions,
) (*Session, error) {
	return parseRPCTurn(ctx, process, descriptor, prompt, plan, opts, acpTurnConfig())
}

func acpTurnConfig() rpcTurnConfig {
	return rpcTurnConfig{
		newMethod:         rpcSessionNew,
		promptMethod:      rpcACPPrompt,
		initializedMethod: rpcACPInitialize,
		handshakeTimeout:  30 * time.Second,
		// ACP 的 session/new 要求 mcpServers 必填（kimi 实测缺省会返回 Invalid params）。
		requiresMCPServers: true,
		promptParams: func(sessionID string, prompt string) map[string]any {
			return map[string]any{
				"sessionId": sessionID,
				"prompt":    []any{map[string]any{"type": "text", "text": prompt}},
			}
		},
		extractText: func(frame rpcFrame) string {
			if stringFromMap(frame, "method") != "session/update" {
				return ""
			}
			update := nestedMap(nestedMap(frame, "params"), "update")
			kind := stringFromMap(update, "sessionUpdate")
			if kind != "" && kind != "agent_message_chunk" {
				return ""
			}
			return textFromAny(nestedMap(update, "content"))
		},
	}
}

// parseRPCTurn 是 A/AS 共用的 Session 构造入口。
func parseRPCTurn(
	ctx context.Context,
	process *Process,
	descriptor Descriptor,
	prompt string,
	plan ResumePlan,
	opts ExecOptions,
	cfg rpcTurnConfig,
) (*Session, error) {
	messages := make(chan Message, 64)
	results := make(chan Result, 1)
	go func() {
		defer close(messages)
		defer close(results)
		result := runRPCTurn(ctx, process, descriptor, prompt, plan, opts, messages, cfg)
		results <- result
		process.Finish()
	}()
	return &Session{Messages: messages, Result: results}, nil
}
