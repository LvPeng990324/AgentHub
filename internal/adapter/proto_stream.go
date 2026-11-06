package adapter

// S 族解析器（§8.8）：claude / codebuddy / qwen / cursor 的 stream-json NDJSON。
// ★finalizeStreamResult fail-closed：必须收到 result 事件才算 completed，否则 failed（防半截气泡）。

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

// 本文件同时提供 4 族解析器共用的宽松 JSON 提取 helper，避免各族重复实现。

// stringFromMap 按候选键取第一个非空字符串。
func stringFromMap(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if raw, ok := values[key]; ok {
			if text, ok := raw.(string); ok && text != "" {
				return text
			}
		}
	}
	return ""
}

// nestedMap 取嵌套对象；键不存在时返回 nil（绝不能返回新空 map，
// 否则 sessionIDFromMap 的递归会在空 map 上无限展开导致 stack overflow）。
func nestedMap(values map[string]any, key string) map[string]any {
	if raw, ok := values[key]; ok {
		if nested, ok := raw.(map[string]any); ok {
			return nested
		}
	}
	return nil
}

// textFromAny 从任意 JSON 值里抽取可见文本（优先 text / delta / content）。
func textFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		for _, key := range []string{"text", "delta", "content", "message", "part"} {
			if raw, ok := typed[key]; ok {
				if text := textFromAny(raw); text != "" {
					return text
				}
			}
		}
	case []any:
		var builder []byte
		for _, item := range typed {
			builder = append(builder, []byte(textFromAny(item))...)
		}
		return string(builder)
	}
	return ""
}

// sessionIDFromMap 从流事件/响应里抽取会话恢复标识（限制递归深度，防御畸形 JSON）。
func sessionIDFromMap(values map[string]any) string {
	return sessionIDFromMapDepth(values, 0)
}

func sessionIDFromMapDepth(values map[string]any, depth int) string {
	if len(values) == 0 || depth > 4 {
		return ""
	}
	if id := stringFromMap(values,
		"session_id", "sessionId", "sessionID", "thread_id", "threadId", "threadID"); id != "" {
		return id
	}
	for _, key := range []string{"result", "data", "params"} {
		nested := nestedMap(values, key)
		if nested == nil {
			continue
		}
		if id := sessionIDFromMapDepth(nested, depth+1); id != "" {
			return id
		}
	}
	return ""
}

// StreamParser 解析 S 族 NDJSON 行流。
type StreamParser struct{}

// NewStreamParser 构造 S 族解析器。
func NewStreamParser() *StreamParser {
	return &StreamParser{}
}

// Parse 启动解析 goroutine，返回流式 Session。
func (p *StreamParser) Parse(
	ctx context.Context,
	process *Process,
	descriptor Descriptor,
	plan ResumePlan,
) (*Session, error) {
	messages := make(chan Message, 64)
	results := make(chan Result, 1)
	go func() {
		defer close(messages)
		defer close(results)
		result, produced := p.parse(ctx, process, descriptor, messages)
		// 会话恢复被拒 → 降级新会话（§8.6 / K12 / U4 / R-18）。
		// 只在「续接请求 + 零输出 + 错误文案命中拒绝特征」时重跑，避免重复输出。
		if shouldDowngradeResume(result, produced, plan, descriptor, process) {
			process.Finish()
			if restarted, restartErr := process.restart(ctx, ""); restartErr == nil {
				fallback, _ := p.parse(ctx, restarted, descriptor, messages)
				fallback.ResumeRejected = true
				results <- fallback
				restarted.Finish()
				return
			}
		}
		results <- result
		process.Finish()
	}()
	return &Session{Messages: messages, Result: results}, nil
}

// shouldDowngradeResume 判断是否应把「恢复被拒」降级为新会话重跑（S/J 两族共用）。
// 三个前提：本轮失败、零文本输出、请求里带了恢复 argv；再叠加拒绝判定
// （文案匹配 或 opencode/codex 这类不可区分类型一律降级）。
// 已产出文本时绝不重跑，否则前端会看到重复内容。
func shouldDowngradeResume(
	result Result,
	produced bool,
	plan ResumePlan,
	descriptor Descriptor,
	process *Process,
) bool {
	if result.Status != ResultFailed || produced || process == nil || process.restart == nil {
		return false
	}
	if len(plan.Args) == 0 {
		return false
	}
	return ResumeRejectionUndetectable(descriptor.Type) ||
		ClassifyResumeFailure(errors.New(result.Error), descriptor)
}

// parse 返回终态与「是否已产出文本」（后者决定能否安全重跑）。
func (p *StreamParser) parse(
	ctx context.Context,
	process *Process,
	descriptor Descriptor,
	out chan<- Message,
) (Result, bool) {
	startedAt := process.StartedAt()
	scanner := newAgentStreamScanner(process.Stdout)
	sessionID := ""
	seenResult := false
	seenDelta := false
	produced := false
	resultFailed := false
	resultError := ""

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return Result{Status: statusForContextErr(ctx.Err()), DurationMs: time.Since(startedAt).Milliseconds()}, produced
		default:
		}
		line := trimTrailingCR(scanner.Text())
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			slog.Debug("S 族忽略非 JSON 行", "type", descriptor.Type, "bytes", len(line))
			continue
		}
		if id := sessionIDFromMap(event); id != "" {
			sessionID = id
		}
		eventType, _ := event["type"].(string)
		switch eventType {
		case "content_block_delta":
			delta := nestedMap(event, "delta")
			text := stringFromMap(delta, "text")
			if text == "" {
				text = textFromAny(delta)
			}
			if text != "" {
				seenDelta = true
				produced = true
				out <- Message{Type: MsgText, Content: text}
			}
		case "assistant":
			// 仅在全程没有 content_block_delta 时兜底取完整文本，避免重复输出。
			if !seenDelta {
				if text := textFromAny(nestedMap(event, "message")); text != "" {
					produced = true
					out <- Message{Type: MsgText, Content: text}
				}
			}
		case "result":
			seenResult = true
			if subtype := stringFromMap(event, "subtype"); subtype != "" && subtype != "success" {
				resultFailed = true
				resultError = stringFromMap(event, "error", "result")
				if resultError == "" {
					resultError = "Agent 返回失败状态"
				}
			}
			// result 事件是协议边界：cursor 发 result 后 worker 可能不退出，主动回收整树。
			process.KillTree()
		case "tool_use", "thinking":
			// Q8：工具/思考过程仅落日志，不进帧。
			slog.Debug("S 族忽略工具/思考事件", "type", descriptor.Type)
		}
		if seenResult {
			break
		}
	}

	duration := time.Since(startedAt).Milliseconds()
	// chat.stop / agent.unregistered 会 cancel ctx 并 KillTree；此时 stdout 关闭属预期收尾，
	// 必须映射为 cancelled（→ chat.done{stopped}），保留已产出内容（R-14）。
	if ctx.Err() != nil {
		return Result{Status: statusForContextErr(ctx.Err()), DurationMs: duration}, produced
	}
	if err := scanner.Err(); err != nil {
		return Result{Status: ResultFailed, Error: "读取 agent 输出失败", DurationMs: duration}, produced
	}
	if resultFailed {
		return Result{Status: ResultFailed, Error: resultError, DurationMs: duration}, produced
	}
	// ★fail-closed：没有 result 事件一律判失败，不让半截输出冒充完成。
	if !seenResult {
		return Result{
			Status:     ResultFailed,
			Error:      "未收到 result 事件，按失败处理",
			DurationMs: duration,
		}, produced
	}
	// 「新会话在 prompt 前失败不得上报 session id」由 fail-closed 保证：
	// 只有成功走到这里才回带 sessionID。
	return Result{Status: ResultCompleted, SessionID: sessionID, DurationMs: duration}, produced
}
