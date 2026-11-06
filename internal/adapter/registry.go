package adapter

// 描述符表（§8.2 / §8.3）：1 张表 + 4 个协议族解析器，
// 新增一种 agent 只需加一行描述符，不复制 backend（D-1 / R-25）。

import (
	"sort"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// ProtocolFamily 是协议族标识；取值来自 pkg/protocol，避免两处定义漂移（K4）。
type ProtocolFamily string

const (
	FamilyStream    ProtocolFamily = protocol.FamilyStream
	FamilyACP       ProtocolFamily = protocol.FamilyACP
	FamilyAppServer ProtocolFamily = protocol.FamilyAppServer
	FamilyJSON      ProtocolFamily = protocol.FamilyJSON
)

// ResumeKind 是会话恢复写法；取值来自 pkg/protocol/frames.go 的 ResumeKind* 常量。
type ResumeKind string

const (
	ResumeArg          ResumeKind = protocol.ResumeKindFlag
	ResumeSessionRPC   ResumeKind = protocol.ResumeKindSessionResume
	ResumeLoadRPC      ResumeKind = protocol.ResumeKindSessionLoad
	ResumeThreadRPC    ResumeKind = protocol.ResumeKindThreadResume
	ResumeSessionIDArg ResumeKind = protocol.ResumeKindSessionID
)

// Descriptor 描述一种 agent CLI 的启动方式、协议族与恢复方式（§8.2 逐字段）。
// 注意：表本身不含可执行路径——路径由 Discoverer 运行时探测后放入 ExecOptions.ExecPath。
type Descriptor struct {
	Type            string         `json:"type"`
	DisplayName     string         `json:"display_name"`
	DefaultCommand  string         `json:"default_command"`
	PathEnvKey      string         `json:"path_env_key"`
	Family          ProtocolFamily `json:"family"`
	Subcommand      []string       `json:"subcommand"`
	LaunchArgs      []string       `json:"launch_args"`
	Resume          ResumeKind     `json:"resume"`
	ResumeArgFlag   string         `json:"resume_arg_flag"`
	WindowsSpecial  bool           `json:"windows_special"`
	ExtraProbePaths []string       `json:"extra_probe_paths"`
	IdleGraceMs     int            `json:"idle_grace_ms"`
	HandshakeMs     int            `json:"handshake_ms"`
}

// Descriptors 是 §8.3 的 9 行静态表（逐行照抄）。
var Descriptors = map[string]Descriptor{
	protocol.AgentTypeClaude: {
		Type:           protocol.AgentTypeClaude,
		DisplayName:    "Claude Code",
		DefaultCommand: "claude",
		PathEnvKey:     "AGENTHUB_CLAUDE_PATH",
		Family:         FamilyStream,
		LaunchArgs: []string{
			"-p", "--output-format", "stream-json", "--input-format", "stream-json",
			"--verbose", "--permission-mode", "bypassPermissions",
			"--disallowedTools", "AskUserQuestion",
		},
		Resume:         ResumeArg,
		ResumeArgFlag:  "--resume",
		WindowsSpecial: true,
	},
	protocol.AgentTypeCodeBuddy: {
		Type:           protocol.AgentTypeCodeBuddy,
		DisplayName:    "CodeBuddy",
		DefaultCommand: "codebuddy",
		PathEnvKey:     "AGENTHUB_CODEBUDDY_PATH",
		Family:         FamilyStream,
		// headless 禁 Plan 三件套：AskUserQuestion / EnterPlanMode / ExitPlanMode
		LaunchArgs: []string{
			"-p", "--output-format", "stream-json", "--input-format", "stream-json",
			"--verbose", "--permission-mode", "bypassPermissions",
			"--disallowedTools", "AskUserQuestion", "EnterPlanMode", "ExitPlanMode",
		},
		Resume:         ResumeArg,
		ResumeArgFlag:  "--resume",
		WindowsSpecial: true,
	},
	protocol.AgentTypeQwen: {
		Type:           protocol.AgentTypeQwen,
		DisplayName:    "Qwen Code",
		DefaultCommand: "qwen",
		PathEnvKey:     "AGENTHUB_QWEN_PATH",
		Family:         FamilyStream,
		LaunchArgs:     []string{"--output-format", "stream-json", "--yolo"},
		Resume:         ResumeArg,
		ResumeArgFlag:  "--resume",
		WindowsSpecial: true,
	},
	protocol.AgentTypeCursor: {
		Type:           protocol.AgentTypeCursor,
		DisplayName:    "Cursor Agent",
		DefaultCommand: "cursor-agent",
		PathEnvKey:     "AGENTHUB_CURSOR_PATH",
		Family:         FamilyStream,
		LaunchArgs: []string{
			"-p", "--output-format", "stream-json", "--yolo", "--workspace", "<cwd>",
		},
		Resume:         ResumeArg,
		ResumeArgFlag:  "--resume",
		WindowsSpecial: true,
	},
	protocol.AgentTypeHermes: {
		Type:           protocol.AgentTypeHermes,
		DisplayName:    "Hermes",
		DefaultCommand: "hermes",
		PathEnvKey:     "AGENTHUB_HERMES_PATH",
		Family:         FamilyACP,
		Subcommand:     []string{"acp"},
		Resume:         ResumeSessionRPC,
		HandshakeMs:    30000,
	},
	protocol.AgentTypeKimi: {
		Type:           protocol.AgentTypeKimi,
		DisplayName:    "Kimi Code",
		DefaultCommand: "kimi",
		PathEnvKey:     "AGENTHUB_KIMI_PATH",
		Family:         FamilyACP,
		Subcommand:     []string{"acp"},
		Resume:         ResumeSessionRPC,
		WindowsSpecial: true, // hideAgentWindow + stderr 4xx/5xx 嗅探
		HandshakeMs:    30000,
	},
	protocol.AgentTypeCodex: {
		Type:           protocol.AgentTypeCodex,
		DisplayName:    "Codex",
		DefaultCommand: "codex",
		PathEnvKey:     "AGENTHUB_CODEX_PATH",
		Family:         FamilyAppServer,
		Subcommand:     []string{"app-server", "--listen", "stdio://"},
		Resume:         ResumeThreadRPC,
		HandshakeMs:    60000, // WSL app-server 冷启动 ~39s
		ExtraProbePaths: []string{
			"/Applications/Codex.app/Contents/MacOS/Codex",
			"C:/Program Files/Codex/Codex.exe",
			"~/.codex/bin/codex",
		},
	},
	protocol.AgentTypeOpencode: {
		Type:           protocol.AgentTypeOpencode,
		DisplayName:    "OpenCode",
		DefaultCommand: "opencode",
		PathEnvKey:     "AGENTHUB_OPENCODE_PATH",
		Family:         FamilyJSON,
		Subcommand:     []string{"run", "--format", "json", "--dangerously-skip-permissions"},
		Resume:         ResumeSessionIDArg,
		ResumeArgFlag:  "--session",
	},
	protocol.AgentTypeOpenclaw: {
		Type:           protocol.AgentTypeOpenclaw,
		DisplayName:    "OpenClaw",
		DefaultCommand: "openclaw",
		PathEnvKey:     "AGENTHUB_OPENCLAW_PATH",
		Family:         FamilyJSON,
		Subcommand:     []string{"agent", "--local", "--json"},
		// --message 占位；prompt 一律走 stdin，绝不进 argv（§8.5）
		LaunchArgs:     []string{"--message", "-"},
		Resume:         ResumeSessionIDArg,
		ResumeArgFlag:  "--session-id",
		WindowsSpecial: true,
		IdleGraceMs:    2000, // 整块 JSON 输出完进程可 8 分钟不退出，必须提前收尾
	},
}

// Registry 提供描述符查询；构造时深拷贝静态表，避免调用方改写全局描述符。
type Registry struct {
	descriptors map[string]Descriptor
}

// NewRegistry 构造描述符表。
func NewRegistry() *Registry {
	copied := make(map[string]Descriptor, len(Descriptors))
	for key, descriptor := range Descriptors {
		descriptor.Subcommand = append([]string(nil), descriptor.Subcommand...)
		descriptor.LaunchArgs = append([]string(nil), descriptor.LaunchArgs...)
		descriptor.ExtraProbePaths = append([]string(nil), descriptor.ExtraProbePaths...)
		copied[key] = descriptor
	}
	return &Registry{descriptors: copied}
}

// Get 按 type 查询描述符。
func (r *Registry) Get(agentType string) (Descriptor, bool) {
	descriptor, ok := r.descriptors[agentType]
	return descriptor, ok
}

// ByFamily 返回某协议族的全部描述符（按 type 排序）。
func (r *Registry) ByFamily(family ProtocolFamily) []Descriptor {
	result := make([]Descriptor, 0)
	for _, descriptor := range r.descriptors {
		if descriptor.Family == family {
			result = append(result, descriptor)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Type < result[j].Type })
	return result
}

// All 返回全部描述符（按 type 排序），供 Discoverer 遍历。
func (r *Registry) All() []Descriptor {
	result := make([]Descriptor, 0, len(r.descriptors))
	for _, descriptor := range r.descriptors {
		result = append(result, descriptor)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Type < result[j].Type })
	return result
}

// Types 返回全部 agent 类型（按字典序）。
func (r *Registry) Types() []string {
	result := make([]string, 0, len(r.descriptors))
	for agentType := range r.descriptors {
		result = append(result, agentType)
	}
	sort.Strings(result)
	return result
}
