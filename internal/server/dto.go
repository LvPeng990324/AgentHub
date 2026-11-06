package server

// 本文件是 REST 契约的唯一出口（K14）：统一响应包络、E01~E15 全部请求/响应 DTO、
// DB 行实体，以及 DB 实体 → DTO 的转换函数。
// 字段名与 docs/openapi.yaml 的 components.schemas 逐字一致（snake_case）。

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/agenthub/agenthub/pkg/protocol"
)

// ---------- 统一响应包络 ----------

// Envelope 是所有 REST 响应的外层结构：{code, message, data}。
// code=0 表示成功；失败时 data 恒为 null，HTTP 状态由 protocol.HTTPStatus 决定。
type Envelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

// OK 构造成功包络。
func OK(data any) Envelope {
	return Envelope{Code: protocol.CodeOK, Message: protocol.ErrorMessage(protocol.CodeOK), Data: data}
}

// Fail 构造失败包络，中文文案取自错误码表。
func Fail(code int) Envelope {
	return Envelope{Code: code, Message: protocol.ErrorMessage(code), Data: nil}
}

// FailMessage 构造失败包络并覆盖默认中文文案（用于附带上下文的错误提示）。
func FailMessage(code int, message string) Envelope {
	if message == "" {
		return Fail(code)
	}
	return Envelope{Code: code, Message: message, Data: nil}
}

// ---------- DB 行实体（与 §5.2 DDL 列一一对应） ----------

// Device 对应 devices 表一行。时间字段为 RFC3339 UTC 文本，空串表示 SQL NULL。
type Device struct {
	ID              string
	Name            string
	OS              string
	Arch            string
	AdapterVersion  string
	ProtocolVersion int
	Remark          string
	Online          bool
	LastSeenAt      string
	CreatedAt       string
	UpdatedAt       string
}

// Agent 对应 agents 表一行；DeviceName / SessionID 由 JOIN 填充，非表内列。
// 三态独立：行存在即 discovered；Registered 持久化；Online 由 WS + 心跳推导。
type Agent struct {
	ID          string
	DeviceID    string
	Type        string
	DisplayName string
	Name        string
	Version     string
	ExecPath    string
	Remark      string
	Registered  bool
	Online      bool
	LastSeenAt  string
	CreatedAt   string
	UpdatedAt   string
	DeviceName  string // LEFT JOIN devices.name
	SessionID   string // LEFT JOIN sessions.id，空串表示尚无会话
}

// Session 对应 sessions 表一行；一个 agent 至多一条（agent_id UNIQUE）。
type Session struct {
	ID        string
	AgentID   string
	ResumeID  string
	CreatedAt string
	UpdatedAt string
}

// Msg 对应 messages 表一行。Seq 是会话级权威序号（L2），平台唯一分配。
type Msg struct {
	ID        int64
	SessionID string
	Seq       int64
	Role      string
	Content   string
	Status    string
	CreatedAt string
}

// ---------- E01 登录 / E03 登录态 ----------

// LoginRequest 是 POST /auth/login 的请求体。
type LoginRequest struct {
	Password string `json:"password"`
}

// LoginData 是登录成功返回的 data。
type LoginData struct {
	Token     string `json:"token"`
	TokenType string `json:"token_type"`
	ExpiresIn int    `json:"expires_in"`
}

// MeData 是 GET /auth/me 返回的 data。
type MeData struct {
	Authenticated bool   `json:"authenticated"`
	ExpiresAt     string `json:"expires_at"`
}

// ---------- E04 meta / E05 health ----------

// MetaData 是 GET /meta 返回的 data。只含公开信息，绝不含密码 / 令牌 / 监听地址。
type MetaData struct {
	Name            string                   `json:"name"`
	Version         string                   `json:"version"`
	Domain          string                   `json:"domain"`
	ProtocolVersion int                      `json:"protocol_version"`
	AgentTypes      []protocol.AgentTypeInfo `json:"agent_types"`
}

// HealthData 是 GET /health 返回的 data。
type HealthData struct {
	Status  string `json:"status"`
	UptimeS int64  `json:"uptime_s"`
	DB      string `json:"db"`
}

// ---------- E06 / E07 设备 ----------

// AgentBriefDTO 是设备页展开项里的 agent 摘要（含 Q14 只读 version / exec_path）。
type AgentBriefDTO struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	Version     string `json:"version"`
	ExecPath    string `json:"exec_path"`
	Registered  bool   `json:"registered"`
	Online      bool   `json:"online"`
}

// DeviceDTO 是完整设备 DTO。
type DeviceDTO struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	OS              string          `json:"os"`
	Arch            string          `json:"arch"`
	AdapterVersion  string          `json:"adapter_version"`
	ProtocolVersion int             `json:"protocol_version"`
	Online          bool            `json:"online"`
	LastSeenAt      *string         `json:"last_seen_at"`
	Remark          string          `json:"remark"`
	CreatedAt       string          `json:"created_at"`
	Agents          []AgentBriefDTO `json:"agents"`
}

// DeviceListData 是 E06 的 data。
type DeviceListData struct {
	Items []DeviceDTO `json:"items"`
	Total int         `json:"total"`
}

// RemarkRequest 是 E07 / E13 的请求体（纯文本备注，≤500 字）。
type RemarkRequest struct {
	Remark string `json:"remark"`
}

// ---------- E08~E13 Agent ----------

// AgentDTO 是 Agent 全量 DTO（三态独立）。
type AgentDTO struct {
	ID          string  `json:"id"`
	DeviceID    string  `json:"device_id"`
	DeviceName  string  `json:"device_name"`
	Type        string  `json:"type"`
	DisplayName string  `json:"display_name"`
	Name        string  `json:"name"`
	Version     string  `json:"version"`
	ExecPath    string  `json:"exec_path"`
	Remark      string  `json:"remark"`
	Registered  bool    `json:"registered"`
	Online      bool    `json:"online"`
	LastSeenAt  *string `json:"last_seen_at"`
	CreatedAt   string  `json:"created_at"`
	SessionID   *string `json:"session_id"`
}

// AgentListData 是 E08 / E09 的 data。
type AgentListData struct {
	Items []AgentDTO `json:"items"`
	Total int        `json:"total"`
}

// BatchRegisterRequest 是 E12 的请求体。
type BatchRegisterRequest struct {
	AgentIDs []string `json:"agent_ids"`
}

// BatchRegisterData 是 E12 的 data；两个数组恒为非 null 空数组。
type BatchRegisterData struct {
	Registered []string `json:"registered"`
	Skipped    []string `json:"skipped"`
}

// UnregisterData 是 E11 的 data。
type UnregisterData struct {
	AgentID         string `json:"agent_id"`
	Registered      bool   `json:"registered"`
	MessagesCleared bool   `json:"messages_cleared"`
}

// ---------- E14 / E15 消息与会话 ----------

// MsgDTO 是历史消息中的一条。
type MsgDTO struct {
	Seq       int64  `json:"seq"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// MessageListData 是 E14 的 data。
type MessageListData struct {
	Items     []MsgDTO `json:"items"`
	HasMore   bool     `json:"has_more"`
	LatestSeq int64    `json:"latest_seq"`
}

// SessionData 是 E15 的 data；无会话时 session_id 为 null。
type SessionData struct {
	SessionID    *string `json:"session_id"`
	ResumeID     string  `json:"resume_id"`
	MessageCount int64   `json:"message_count"`
}

// ---------- 查询参数 ----------

// ListQuery 汇总列表端点与历史消息端点的查询参数。
// 指针字段用于区分「未传」与「显式传 false」两种语义。
type ListQuery struct {
	Q          string
	Type       string
	DeviceID   string
	Online     *bool
	Registered *bool
	Limit      int
	Offset     int
	SinceSeq   int64
}

// DefaultListLimit / MaxListLimit 对应 openapi.yaml 的 Limit 参数约束。
const (
	DefaultListLimit = 100
	MaxListLimit     = 500
)

// ParseListQuery 从请求中解析并夹紧分页 / 筛选参数；非法值一律回退到默认值。
func ParseListQuery(r *http.Request) ListQuery {
	query := r.URL.Query()
	result := ListQuery{
		Q:        strings.TrimSpace(query.Get("q")),
		Type:     strings.TrimSpace(query.Get("type")),
		DeviceID: strings.TrimSpace(query.Get("device_id")),
		Limit:    DefaultListLimit,
		Offset:   0,
		SinceSeq: 0,
	}
	if raw := query.Get("limit"); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil {
			result.Limit = clamp(value, 1, MaxListLimit)
		}
	}
	if raw := query.Get("offset"); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value >= 0 {
			result.Offset = value
		}
	}
	if raw := query.Get("since_seq"); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value >= 0 {
			result.SinceSeq = value
		}
	}
	result.Online = parseBoolPtr(query.Get("online"))
	result.Registered = parseBoolPtr(query.Get("registered"))
	return result
}

// parseBoolPtr 把 "true/false/1/0" 解析为 *bool；空串或非法值返回 nil（不筛选）。
func parseBoolPtr(raw string) *bool {
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return nil
	}
	return &value
}

// clamp 把 value 限制在 [low, high] 区间内。
func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// ---------- DB 实体 → DTO 转换 ----------

// NullableString 把空串转为 JSON null，非空串转为指针（用于 last_seen_at / session_id）。
func NullableString(value string) *string {
	if value == "" {
		return nil
	}
	copyValue := value
	return &copyValue
}

// ToAgentBriefDTO 把 Agent 行转换为设备页内嵌摘要。
func ToAgentBriefDTO(agent Agent) AgentBriefDTO {
	return AgentBriefDTO{
		ID:          agent.ID,
		Type:        agent.Type,
		DisplayName: agent.DisplayName,
		Version:     agent.Version,
		ExecPath:    agent.ExecPath,
		Registered:  agent.Registered,
		Online:      agent.Online,
	}
}

// ToDeviceDTO 把 Device 行与已组装好的 agent 摘要转换为完整设备 DTO。
func ToDeviceDTO(device Device, briefs []AgentBriefDTO) DeviceDTO {
	if briefs == nil {
		briefs = make([]AgentBriefDTO, 0)
	}
	return DeviceDTO{
		ID:              device.ID,
		Name:            device.Name,
		OS:              device.OS,
		Arch:            device.Arch,
		AdapterVersion:  device.AdapterVersion,
		ProtocolVersion: device.ProtocolVersion,
		Online:          device.Online,
		LastSeenAt:      NullableString(device.LastSeenAt),
		Remark:          device.Remark,
		CreatedAt:       device.CreatedAt,
		Agents:          briefs,
	}
}

// ToAgentDTO 把 Agent 行（含 JOIN 字段）转换为全量 Agent DTO。
func ToAgentDTO(agent Agent) AgentDTO {
	return AgentDTO{
		ID:          agent.ID,
		DeviceID:    agent.DeviceID,
		DeviceName:  agent.DeviceName,
		Type:        agent.Type,
		DisplayName: agent.DisplayName,
		Name:        agent.Name,
		Version:     agent.Version,
		ExecPath:    agent.ExecPath,
		Remark:      agent.Remark,
		Registered:  agent.Registered,
		Online:      agent.Online,
		LastSeenAt:  NullableString(agent.LastSeenAt),
		CreatedAt:   agent.CreatedAt,
		SessionID:   NullableString(agent.SessionID),
	}
}

// ToMsgDTO 把 messages 行转换为历史消息 DTO。
func ToMsgDTO(message Msg) MsgDTO {
	return MsgDTO{
		Seq:       message.Seq,
		Role:      message.Role,
		Content:   message.Content,
		Status:    message.Status,
		CreatedAt: message.CreatedAt,
	}
}
