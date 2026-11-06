// Package protocol 是 AgentHub 平台服务与设备侧适配器之间唯一的共享协议包。
//
// 本包为纯 Go、零内部依赖：WebSocket 帧信封、F01~F15 帧类型常量、帧 payload 结构、
// REST/WS 共用错误码、跨端共享枚举与 9 种 Agent 类型描述符均在此单一定义（K4 / K14）。
// 任何其它包不得硬编码帧名字符串字面量或错误码数字字面量。
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Message 是双向 WebSocket 帧信封，字段名与 Web 前端 types.ts 严格一致。
//
// Seq 是「连接级」序号（L1）：由每条连接的发送方从 1 开始单调递增，重连重置为 1，
// 接收方据此去重与排序。切勿与下列两者混淆：
//   - 会话级 messages.seq（L2，user_seq / assistant_seq）：平台唯一权威分配、落库、since_seq 补齐；
//   - chunk_index（L3）：单条助手消息内的增量拼接序号。
type Message struct {
	Type    string          `json:"type"`
	Seq     int64           `json:"seq"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// F01~F15 帧类型常量（§7.2）。这是帧名的唯一事实来源。
const (
	FrameDeviceRegister    = "device.register"     // F01 适配器 → 平台，全量快照
	FrameDeviceRegisterAck = "device.register_ack" // F02 平台 → 适配器
	FrameDeviceHeartbeat   = "device.heartbeat"    // F03 适配器 → 平台
	FrameHeartbeatAck      = "heartbeat_ack"       // F04 平台 → 适配器
	FrameChatSend          = "chat.send"           // F05 浏览器 → 平台 → 适配器
	FrameChatSendAck       = "chat.send_ack"       // F06 平台 → 浏览器
	FrameChatChunk         = "chat.chunk"          // F07 适配器 → 平台 → 浏览器
	FrameChatDone          = "chat.done"           // F08 适配器 → 平台 → 浏览器
	FrameChatError         = "chat.error"          // F09 适配器/平台 → 浏览器
	FrameChatStop          = "chat.stop"           // F10 浏览器 → 平台 → 适配器
	FrameAgentStatus       = "agent.status"        // F11 平台 → 浏览器
	FrameAgentUnregistered = "agent.unregistered"  // F12 平台 → 适配器
	FrameAgentRegistered   = "agent.registered"    // F13 平台 → 适配器（可选 P1）
	FrameClientReady       = "client.ready"        // F14 平台 → 浏览器
	FrameDeviceStatus      = "device.status"       // F15 平台 → 浏览器（可选 P1）
)

// allFrameTypes 按 F01~F15 顺序登记全部帧名，供完整性校验与单测使用。
var allFrameTypes = []string{
	FrameDeviceRegister,
	FrameDeviceRegisterAck,
	FrameDeviceHeartbeat,
	FrameHeartbeatAck,
	FrameChatSend,
	FrameChatSendAck,
	FrameChatChunk,
	FrameChatDone,
	FrameChatError,
	FrameChatStop,
	FrameAgentStatus,
	FrameAgentUnregistered,
	FrameAgentRegistered,
	FrameClientReady,
	FrameDeviceStatus,
}

// AllFrameTypes 返回 F01~F15 全部帧名的副本（按编号顺序）。
func AllFrameTypes() []string {
	out := make([]string, len(allFrameTypes))
	copy(out, allFrameTypes)
	return out
}

// IsValidFrameType 判断给定帧名是否属于已冻结的 F01~F15 协议族。
func IsValidFrameType(frameType string) bool {
	for _, known := range allFrameTypes {
		if frameType == known {
			return true
		}
	}
	return false
}

// 协议版本与传输层参数（§7.5 / U6），供 server 与 adapter 共同引用。
const (
	// ProtocolVersion 是首版握手协商版本，不匹配时返回 5002 / HTTP 426。
	ProtocolVersion = 1
	// HeartbeatIntervalSeconds 是适配器心跳周期，register_ack 下发给适配器。
	HeartbeatIntervalSeconds = 15
	// MaxPromptBytes 是单条 prompt / 消息内容上限（64 KiB），超出返回 4002。
	MaxPromptBytes = 64 * 1024
	// WSReadLimitBytes 是单条 WS 业务帧读取上限（1 MiB）。
	WSReadLimitBytes = 1024 * 1024
	// MaxRemarkRunes 是设备 / agent 备注的字符上限（U7）。
	MaxRemarkRunes = 500
)

// NewMessage 构造一个带 payload 的帧；payload 为 nil 时下发空对象 {}。
// 返回的 error 只在 payload 无法被 JSON 序列化时出现，调用方必须处理。
func NewMessage(frameType string, seq int64, payload any) (Message, error) {
	if frameType == "" {
		return Message{}, errors.New("帧类型不能为空")
	}
	if !IsValidFrameType(frameType) {
		return Message{}, fmt.Errorf("未知帧类型: %s", frameType)
	}
	raw := json.RawMessage("{}")
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return Message{}, fmt.Errorf("序列化帧 %s 的 payload 失败: %w", frameType, err)
		}
		raw = encoded
	}
	return Message{Type: frameType, Seq: seq, Payload: raw}, nil
}

// Encode 把帧序列化为 JSON 字节；序列化失败时返回 nil。
// 该签名对齐系统设计类图；需要显式错误信息的调用方请使用 Marshal。
func (m Message) Encode() []byte {
	encoded, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return encoded
}

// Marshal 把帧序列化为 JSON 字节并返回显式错误（供 WS 写泵严格处理）。
func (m Message) Marshal() ([]byte, error) {
	encoded, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("序列化帧 %s 失败: %w", m.Type, err)
	}
	return encoded, nil
}

// Decode 把 JSON 字节反序列化为帧信封，并校验必需的 type 字段。
// 帧名白名单校验由分派器通过 IsValidFrameType 完成，此处不拦截未知扩展帧。
func (m *Message) Decode(data []byte) error {
	if m == nil {
		return errors.New("帧信封接收者为空")
	}
	if len(data) == 0 {
		return errors.New("帧内容为空")
	}
	if err := json.Unmarshal(data, m); err != nil {
		return fmt.Errorf("解析帧信封失败: %w", err)
	}
	if m.Type == "" {
		return errors.New("帧缺少 type 字段")
	}
	return nil
}

// DecodePayload 把帧的 payload 反序列化为目标结构体。
// payload 缺省 / null 时保留目标零值且不算错误。
func (m Message) DecodePayload(target any) error {
	if target == nil {
		return errors.New("payload 接收者为空")
	}
	if len(m.Payload) == 0 || string(m.Payload) == "null" {
		return nil
	}
	if err := json.Unmarshal(m.Payload, target); err != nil {
		return fmt.Errorf("解析帧 %s 的 payload 失败: %w", m.Type, err)
	}
	return nil
}

// 9 种 Agent 类型标识（§8.3 / openapi.yaml enum）。
const (
	AgentTypeClaude    = "claude"
	AgentTypeCodeBuddy = "codebuddy"
	AgentTypeQwen      = "qwen"
	AgentTypeCursor    = "cursor"
	AgentTypeHermes    = "hermes"
	AgentTypeKimi      = "kimi"
	AgentTypeCodex     = "codex"
	AgentTypeOpencode  = "opencode"
	AgentTypeOpenclaw  = "openclaw"
)

// 4 个协议族标识（D-1）。
const (
	FamilyStream    = "stream"
	FamilyACP       = "acp"
	FamilyAppServer = "app-server"
	FamilyJSON      = "json"
)

// AgentTypeInfo 是 /api/v1/meta 返回的 agent_types 元素，
// 同时也是 Web 前端 TypeBadge 配色映射的键来源。
type AgentTypeInfo struct {
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	Family      string `json:"family"`
}

// agentTypeTable 是 9 行静态描述符（§8.3 逐行照抄 type / display_name / family）。
var agentTypeTable = []AgentTypeInfo{
	{Type: AgentTypeClaude, DisplayName: "Claude Code", Family: FamilyStream},
	{Type: AgentTypeCodeBuddy, DisplayName: "CodeBuddy", Family: FamilyStream},
	{Type: AgentTypeQwen, DisplayName: "Qwen Code", Family: FamilyStream},
	{Type: AgentTypeCursor, DisplayName: "Cursor Agent", Family: FamilyStream},
	{Type: AgentTypeHermes, DisplayName: "Hermes", Family: FamilyACP},
	{Type: AgentTypeKimi, DisplayName: "Kimi Code", Family: FamilyACP},
	{Type: AgentTypeCodex, DisplayName: "Codex", Family: FamilyAppServer},
	{Type: AgentTypeOpencode, DisplayName: "OpenCode", Family: FamilyJSON},
	{Type: AgentTypeOpenclaw, DisplayName: "OpenClaw", Family: FamilyJSON},
}

// AgentTypes 返回 9 种 Agent 类型描述符的副本，避免调用方改写全局表。
func AgentTypes() []AgentTypeInfo {
	out := make([]AgentTypeInfo, len(agentTypeTable))
	copy(out, agentTypeTable)
	return out
}

// IsValidAgentType 判断类型是否属于 9 种已支持 Agent。
func IsValidAgentType(agentType string) bool {
	for _, item := range agentTypeTable {
		if item.Type == agentType {
			return true
		}
	}
	return false
}
