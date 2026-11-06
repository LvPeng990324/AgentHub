package adapter

// Backend / Session / Message / Result 四件套（§8.1）：把 9 种 CLI 收敛成一个流式 channel。
// 只保留 text / error / status 三类事件，删掉 Multica 的 usage / MCP / ThinkingLevel / Supplement。

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MessageType 是解析器产出的统一事件类型。
type MessageType string

const (
	MsgText   MessageType = "text" // 首版唯一展示类型（Q8：工具/思考仅落日志）
	MsgError  MessageType = "error"
	MsgStatus MessageType = "status" // 如 session_started，可携带 SessionID
)

// Message 是协议族解析器产出的统一事件。
type Message struct {
	Type      MessageType `json:"type"`
	Content   string      `json:"content"`
	SessionID string      `json:"session_id,omitempty"`
}

// Result 是唯一终态，由 Result 通道发送一次后关闭。
type Result struct {
	Status         string `json:"status"` // completed|failed|aborted|timeout|cancelled
	Output         string `json:"output"`
	Error          string `json:"error,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	ResumeRejected bool   `json:"resume_rejected,omitempty"`
	DurationMs     int64  `json:"duration_ms"`
}

// 内部 Result.Status 取值（与 chat.done 的终态拼写不同，由 session.go 映射）。
const (
	ResultCompleted = "completed"
	ResultFailed    = "failed"
	ResultAborted   = "aborted"
	ResultTimeout   = "timeout"
	ResultCancelled = "cancelled"
)

// statusForContextErr 把 ctx 结束原因映射为终态：
// 超时（DeadlineExceeded）→ ResultTimeout（触发 chat.error{5005}）；
// 其余取消（chat.stop / F12 取消注册 / 适配器退出）→ ResultCancelled（保留已产出内容）。
// 四族解析器统一走这里，避免把「超时」误当「用户停止」。
func statusForContextErr(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return ResultTimeout
	}
	return ResultCancelled
}

// Session 是 Execute 的返回：Messages 在 Result 之前关闭，Result 恰好一个值。
type Session struct {
	Messages <-chan Message
	Result   <-chan Result
}

// ExecOptions 是单轮执行参数。
// AgentType / ExecPath 是 §8.1 注释里 backendImpl.Execute 依赖但结构体未列出的运行时字段，
// 由 SessionManager 从 chat.send 的 agent_id 与 Discoverer 快照注入。
type ExecOptions struct {
	AgentType        string
	ExecPath         string
	Cwd              string
	Model            string
	Timeout          time.Duration
	HandshakeTimeout time.Duration
	ResumeSessionID  string
	CustomArgs       []string
}

// Backend 由 backendImpl 按 Descriptor.Family 分派到 4 个解析器之一实现。
type Backend interface {
	Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error)
}

// backendImpl 是统一入口：BuildResume → Launcher.Start → writePrompt(stdin) → 按族分派解析器。
type backendImpl struct {
	registry   *Registry
	launcher   *Launcher
	stream     *StreamParser
	acp        *ACPParser
	appServer  *AppServerParser
	jsonParser *JSONParser
}

// NewBackend 构造统一 Backend（4 个解析器各一份，不是 9 个类型各一份）。
func NewBackend(registry *Registry, launcher *Launcher) Backend {
	return &backendImpl{
		registry:   registry,
		launcher:   launcher,
		stream:     NewStreamParser(),
		acp:        NewACPParser(),
		appServer:  NewAppServerParser(),
		jsonParser: NewJSONParser(),
	}
}

// Execute 启动一个 CLI turn 并返回流式 Session。
func (b *backendImpl) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	descriptor, ok := b.registry.Get(opts.AgentType)
	if !ok {
		return nil, fmt.Errorf("未知 agent 类型: %s", opts.AgentType)
	}
	if opts.ExecPath == "" {
		return nil, fmt.Errorf("agent %s 尚未探测到可执行路径", opts.AgentType)
	}
	if opts.HandshakeTimeout == 0 && descriptor.HandshakeMs > 0 {
		opts.HandshakeTimeout = time.Duration(descriptor.HandshakeMs) * time.Millisecond
	}

	plan := BuildResume(descriptor, opts.ResumeSessionID)
	process, err := b.launcher.Start(ctx, descriptor, opts, plan)
	if err != nil {
		return nil, fmt.Errorf("启动 agent %s 失败: %w", opts.AgentType, err)
	}

	switch descriptor.Family {
	case FamilyStream:
		// S 族：prompt 一律走 stdin，写完即关闭（argv 不含 prompt）。
		process.WritePrompt(prompt)
		return b.stream.Parse(ctx, process, descriptor, plan)
	case FamilyJSON:
		process.WritePrompt(prompt)
		return b.jsonParser.Parse(ctx, process, descriptor, plan)
	case FamilyACP:
		// A/AS 族需要双向 JSON-RPC：stdin 由解析器自己管理，prompt 放进 RPC params。
		return b.acp.Parse(ctx, process, descriptor, prompt, plan, opts)
	case FamilyAppServer:
		return b.appServer.Parse(ctx, process, descriptor, prompt, plan, opts)
	default:
		process.KillTree()
		return nil, fmt.Errorf("不支持的协议族: %s", descriptor.Family)
	}
}
