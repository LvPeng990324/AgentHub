package adapter

// 统一会话恢复入口（§8.6 / D-1）：5 种写法由描述符表声明，全族共用一个 BuildResume。
// ★适配器不持久化 resume_id：它来自平台 chat.send，经 chat.done 回带新值后由平台落库（§8.7）。

import (
	"errors"
	"regexp"
	"strings"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// RPC 方法名（与 pkg/protocol 的 ResumeKind 常量对应，避免硬编码漂移）。
const (
	rpcSessionNew  = "session/new"
	rpcThreadStart = "thread/start"
	rpcInitialize  = "initialize"
	// ACP 规范：initialize 之后客户端必须发 notifications/initialized（kimi 依赖它）。
	rpcACPInitialize = "notifications/initialized"
	rpcACPPrompt     = "session/prompt"
	rpcASTurnStart   = "turn/start"
)

// ResumePlan 描述本轮是「新建」还是「续接」以及续接的 argv / RPC 参数。
type ResumePlan struct {
	Args   []string       `json:"args"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// BuildResume 生成恢复计划。
// sessionID 为空 → 新建计划（Args 空；ACP=session/new，AS=thread/start）；
// 非空 → 按 d.Resume 的 5 种写法分发。
func BuildResume(descriptor Descriptor, sessionID string) ResumePlan {
	plan := ResumePlan{Args: []string{}, Params: map[string]any{}}
	if sessionID == "" {
		switch descriptor.Family {
		case FamilyACP:
			plan.Method = rpcSessionNew
		case FamilyAppServer:
			plan.Method = rpcThreadStart
		}
		return plan
	}
	switch descriptor.Resume {
	case ResumeArg, ResumeSessionIDArg:
		flag := descriptor.ResumeArgFlag
		if flag == "" {
			flag = string(descriptor.Resume)
		}
		plan.Args = []string{flag, sessionID}
	case ResumeSessionRPC:
		plan.Method = protocol.ResumeKindSessionResume
		// cwd 由 proto_acp.go 基于 ExecOptions.Cwd 统一补齐，这里**不写** cwd，
		// 避免出现两处来源（且绝不能硬编码空串 —— kimi 会拒绝 workspace root）。
		plan.Params = map[string]any{"sessionId": sessionID}
	case ResumeLoadRPC:
		// 本期 9 种 agent 均未使用，保留 P2 扩展枚举（Kiro/Grok/Trae/Dim）。
		plan.Method = protocol.ResumeKindSessionLoad
		plan.Params = map[string]any{"sessionId": sessionID}
	case ResumeThreadRPC:
		plan.Method = protocol.ResumeKindThreadResume
		plan.Params = map[string]any{"threadId": sessionID}
	}
	return plan
}

// NewSessionPlan 返回降级用的「新建」计划。
func NewSessionPlan(descriptor Descriptor) ResumePlan {
	return BuildResume(descriptor, "")
}

// resumeRejectPattern 覆盖常见 CLI 的恢复拒绝文案（照抄调研 §4 结论）。
var resumeRejectPattern = regexp.MustCompile(
	`(?i)(invalid session|session not found|unknown session|no such session|` +
		`session (?:expired|invalid|does not exist|not found)|thread not found|invalid thread|` +
		`resume.{0,20}(?:failed|rejected|invalid)|not resumable|failed to resume)`,
)

// ResumeRejectionUndetectable 报告该类型无法从错误文案区分「恢复被拒」与其他启动失败（U4），
// 因此任何失败都一律按恢复被拒降级为新会话，避免无限重试同一个死 id。
// 首版集合：opencode（依据设计 §14 U4）；codex 由 AS 族的 thread/resume 失败分类自理。
func ResumeRejectionUndetectable(agentType string) bool {
	switch agentType {
	case protocol.AgentTypeOpencode:
		return true
	default:
		return false
	}
}

// ClassifyResumeFailure 判断一次失败是否属于「恢复被拒」。
// opencode / codex 的恢复拒绝不可区分，任何失败一律降级新会话（U4 / R-18）。
func ClassifyResumeFailure(err error, descriptor Descriptor) bool {
	if err == nil {
		return false
	}
	if descriptor.Resume == ResumeSessionIDArg || descriptor.Resume == ResumeThreadRPC ||
		ResumeRejectionUndetectable(descriptor.Type) {
		return true
	}
	return resumeRejectPattern.MatchString(strings.ToLower(err.Error()))
}

// IsResumeRejected 是 ClassifyResumeFailure 的 error 文本重载，便于解析器直接用字符串判定。
func IsResumeRejected(message string, descriptor Descriptor) bool {
	if strings.TrimSpace(message) == "" {
		return false
	}
	return ClassifyResumeFailure(errors.New(message), descriptor)
}
