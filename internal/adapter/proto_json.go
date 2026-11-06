package adapter

// J 族解析器（§8.8）：opencode 行式 JSON + openclaw 整块 JSON（idle grace 提前收尾）。
// ★openclaw 绝不能 io.ReadAll 等 EOF——实测结果输出完进程可 8 分钟不退出。

import (
	"context"
	"encoding/json"
	"time"

	"github.com/agenthub/agenthub/pkg/protocol"
)

const jsonTurnAbsoluteLimit = 5 * time.Minute

// JSONParser 按描述符选择行式或整块模式。
type JSONParser struct{}

// NewJSONParser 构造 J 族解析器。
func NewJSONParser() *JSONParser {
	return &JSONParser{}
}

// Parse 启动解析 goroutine，返回流式 Session。
func (p *JSONParser) Parse(
	ctx context.Context,
	process *Process,
	descriptor Descriptor,
	plan ResumePlan,
) (*Session, error) {
	messages := make(chan Message, 64)
	results := make(chan Result, 1)
	run := func(proc *Process) (Result, bool) {
		if descriptor.Type == protocol.AgentTypeOpenclaw {
			return p.parseOpenclaw(ctx, proc, descriptor, messages)
		}
		return p.parseOpencodeLines(ctx, proc, descriptor, messages)
	}
	go func() {
		defer close(messages)
		defer close(results)
		result, produced := run(process)
		// 会话恢复被拒 → 降级新会话（与 S 族共用同一套判定与重跑逻辑）。
		if shouldDowngradeResume(result, produced, plan, descriptor, process) {
			process.Finish()
			if restarted, restartErr := process.restart(ctx, ""); restartErr == nil {
				fallback, _ := run(restarted)
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

// parseOpencodeLines 逐行解析行式 JSON；EOF 是正常协议边界。
func (p *JSONParser) parseOpencodeLines(
	ctx context.Context,
	process *Process,
	descriptor Descriptor,
	out chan<- Message,
) (Result, bool) {
	startedAt := process.StartedAt()
	scanner := newAgentStreamScanner(process.Stdout)
	sessionID := ""
	produced := false
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
		if !json.Valid([]byte(line)) {
			// 非 JSON 行当作纯文本输出（部分 CLI 会混入日志行）。
			produced = true
			out <- Message{Type: MsgText, Content: line}
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if text := textFromAny(event); text != "" {
			produced = true
			out <- Message{Type: MsgText, Content: text}
		}
		if id := sessionIDFromMap(event); id != "" {
			sessionID = id
		}
		if kind := stringFromMap(event, "type", "event"); kind == "result" || kind == "done" || kind == "finish" {
			process.KillTree()
			break
		}
	}
	duration := time.Since(startedAt).Milliseconds()
	if ctx.Err() != nil {
		return Result{Status: statusForContextErr(ctx.Err()), DurationMs: duration}, produced
	}
	if err := scanner.Err(); err != nil {
		return Result{Status: ResultFailed, Error: "读取 opencode 输出失败", DurationMs: duration}, produced
	}
	// 零输出视为失败（CLI 直接退出、恢复被拒等）：否则用户会收到一条空气泡，
	// 「恢复被拒 → 降级新会话」也无从触发。
	if !produced {
		return Result{Status: ResultFailed, Error: "opencode 未返回任何输出", DurationMs: duration}, false
	}
	return Result{Status: ResultCompleted, SessionID: sessionID, DurationMs: duration}, produced
}

// parseOpenclaw 整块 JSON：stdout 静默 IdleGraceMs 即收尾，不等进程退出。
func (p *JSONParser) parseOpenclaw(
	ctx context.Context,
	process *Process,
	descriptor Descriptor,
	out chan<- Message,
) (Result, bool) {
	startedAt := process.StartedAt()
	grace := time.Duration(descriptor.IdleGraceMs) * time.Millisecond
	if grace <= 0 {
		grace = 2 * time.Second
	}
	dataCh := make(chan []byte, 64)
	stopRead := make(chan struct{})
	defer close(stopRead)
	go func() {
		buffer := make([]byte, 4096)
		for {
			count, err := process.Stdout.Read(buffer)
			if count > 0 {
				chunk := append([]byte(nil), buffer[:count]...)
				select {
				case dataCh <- chunk:
				case <-stopRead:
					return
				}
			}
			if err != nil {
				select {
				case <-stopRead:
				default:
					close(dataCh)
				}
				return
			}
		}
	}()

	var accumulated []byte
	var parsed map[string]any
	sessionID := ""
	text := ""
	timer := time.NewTimer(grace)
	defer timer.Stop()
	absolute := time.NewTimer(jsonTurnAbsoluteLimit)
	defer absolute.Stop()

	for {
		select {
		case <-ctx.Done():
			return Result{Status: statusForContextErr(ctx.Err()), DurationMs: time.Since(startedAt).Milliseconds()}, false
		case chunk, ok := <-dataCh:
			if !ok {
				if ctx.Err() != nil {
					return Result{Status: statusForContextErr(ctx.Err()), DurationMs: time.Since(startedAt).Milliseconds()}, false
				}
				return finalizeJSONTurn(parsed, text, sessionID, startedAt, out)
			}
			accumulated = append(accumulated, chunk...)
			if json.Valid(accumulated) {
				var event map[string]any
				if err := json.Unmarshal(accumulated, &event); err == nil {
					parsed = event
					sessionID = sessionIDFromMap(event)
					text = textFromAny(event)
				}
			}
			resetTimer(timer, grace)
		case <-timer.C:
			// ★静默宽限到点：无论进程是否退出都收尾，绝不等待 EOF。
			return finalizeJSONTurn(parsed, text, sessionID, startedAt, out)
		case <-absolute.C:
			return finalizeJSONTurn(parsed, text, sessionID, startedAt, out)
		}
	}
}

// finalizeJSONTurn 在收尾时把已解析文本一次性投递并生成终态。
func finalizeJSONTurn(
	parsed map[string]any,
	text string,
	sessionID string,
	startedAt time.Time,
	out chan<- Message,
) (Result, bool) {
	duration := time.Since(startedAt).Milliseconds()
	if parsed == nil {
		return Result{
			Status:     ResultFailed,
			Error:      "未收到可解析的整块 JSON 结果",
			DurationMs: duration,
		}, false
	}
	produced := false
	if text != "" {
		produced = true
		out <- Message{Type: MsgText, Content: text}
	}
	return Result{Status: ResultCompleted, SessionID: sessionID, DurationMs: duration}, produced
}

// resetTimer 安全地重置 timer（先 Stop 并排空，再 Reset）。
func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}
