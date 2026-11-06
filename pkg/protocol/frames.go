package protocol

// 本文件定义 F01~F15 全部帧的 payload 结构体（§7.3 逐帧字段），
// 以及跨端共享的枚举常量。JSON 字段一律 snake_case（K2）。

// ---------- 跨端共享枚举 ----------

// 消息角色，对应 messages.role。
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
)

// 落库消息状态，对应 messages.status（openapi.yaml Msg.status）。
// 流式占位为 streaming；chat.done 落为 complete / stopped / interrupted；出错为 error。
const (
	MessageStatusStreaming   = "streaming"
	MessageStatusComplete    = "complete"
	MessageStatusError       = "error"
	MessageStatusStopped     = "stopped"
	MessageStatusInterrupted = "interrupted"
)

// chat.done 的终态取值（与 MessageStatus 语义同义但拼写不同，禁止混用）。
const (
	DoneStatusCompleted   = "completed"
	DoneStatusStopped     = "stopped"
	DoneStatusInterrupted = "interrupted"
	DoneStatusFailed      = "failed"
)

// agent.status / device.status 的状态变化原因。
const (
	ReasonDeviceOnline     = "device_online"
	ReasonDeviceOffline    = "device_offline"
	ReasonRegistered       = "registered"
	ReasonUnregistered     = "unregistered"
	ReasonSnapshot         = "snapshot"
	ReasonStaleStreaming   = "stale_streaming"
	ReasonAdapterReconnect = "adapter_reconnect"
)

// 适配器上报的 agent 简态。
const (
	AgentStatusAvailable   = "available"
	AgentStatusUnavailable = "unavailable"
)

// 会话恢复方式（§8.3 resume 列），供 T04 的 BuildResume 使用。
const (
	ResumeKindFlag          = "--resume"
	ResumeKindSessionResume = "session/resume"
	ResumeKindThreadResume  = "thread/resume"
	ResumeKindSessionID     = "--session-id"
	ResumeKindSessionLoad   = "session/load"
)

// ---------- F01 device.register（适配器 → 平台） ----------

// AgentSnapshot 是全量快照中的单个 agent 条目。
// 快照以 type 为准，平台据此拼出稳定键 agent_id = "<device_id>:<type>"。
type AgentSnapshot struct {
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	Version     string `json:"version"`
	ExecPath    string `json:"exec_path"`
	Status      string `json:"status"`
}

// DeviceRegisterPayload 是适配器连接成功后立即发送的全量快照（幂等 upsert）。
type DeviceRegisterPayload struct {
	DeviceID        string          `json:"device_id"`
	Name            string          `json:"name"`
	OS              string          `json:"os"`
	Arch            string          `json:"arch"`
	AdapterVersion  string          `json:"adapter_version"`
	ProtocolVersion int             `json:"protocol_version"`
	Agents          []AgentSnapshot `json:"agents"`
}

// ---------- F02 device.register_ack（平台 → 适配器） ----------

// DeviceRegisterAckPayload 回已注册 agent 集合、版本与心跳参数。
type DeviceRegisterAckPayload struct {
	DeviceID           string   `json:"device_id"`
	Accepted           bool     `json:"accepted"`
	ServerVersion      string   `json:"server_version"`
	ProtocolVersion    int      `json:"protocol_version"`
	HeartbeatIntervalS int      `json:"heartbeat_interval_s"`
	RegisteredAgentIDs []string `json:"registered_agent_ids"`
}

// ---------- F03 device.heartbeat / F04 heartbeat_ack ----------

// HeartbeatAgent 是心跳里的 agent 简态。
type HeartbeatAgent struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// HeartbeatPayload 是 15s 心跳帧。
type HeartbeatPayload struct {
	DeviceID string           `json:"device_id"`
	Agents   []HeartbeatAgent `json:"agents"`
}

// HeartbeatAckPayload 回服务端时间（RFC3339 UTC）。
type HeartbeatAckPayload struct {
	ServerTime string `json:"server_time"`
}

// ---------- F05 chat.send（浏览器 → 平台 → 适配器） ----------

// ChatSendPayload 同时覆盖两个方向：
//   - 浏览器 → 平台：只有 agent_id / content；
//   - 平台 → 适配器：补齐 session_id / user_seq / assistant_seq / resume_id。
//
// 平台补充字段用 omitempty，浏览器上行帧中缺省即保持零值；
// resume_id 不用 omitempty，空串明确表示「首轮 / 无需恢复」（§7.3）。
type ChatSendPayload struct {
	AgentID      string `json:"agent_id"`
	Content      string `json:"content"`
	SessionID    string `json:"session_id,omitempty"`
	UserSeq      int64  `json:"user_seq,omitempty"`
	AssistantSeq int64  `json:"assistant_seq,omitempty"`
	ResumeID     string `json:"resume_id"`
}

// ---------- F06 chat.send_ack（平台 → 浏览器） ----------

// ChatSendAckPayload 确认已接收并分配会话级 user_seq / assistant_seq。
type ChatSendAckPayload struct {
	AgentID      string `json:"agent_id"`
	SessionID    string `json:"session_id"`
	UserSeq      int64  `json:"user_seq"`
	AssistantSeq int64  `json:"assistant_seq"`
	CreatedAt    string `json:"created_at"`
}

// ---------- F07 chat.chunk（适配器 → 平台 → 浏览器） ----------

// ChatChunkPayload 是流式增量；chunk_index 从 0 单调递增，供前端去重拼接。
// 平台原样透传给浏览器，仅重分配连接级 seq；chunk 不逐条落库。
type ChatChunkPayload struct {
	AgentID      string `json:"agent_id"`
	SessionID    string `json:"session_id"`
	AssistantSeq int64  `json:"assistant_seq"`
	ChunkIndex   int    `json:"chunk_index"`
	Delta        string `json:"delta"`
}

// ---------- F08 chat.done（适配器 → 平台 → 浏览器） ----------

// ChatDonePayload 是终态帧；平台在此一次性落库助手消息完整文本。
// ResumeID 空串表示不更新 sessions.resume_id。
type ChatDonePayload struct {
	AgentID        string `json:"agent_id"`
	SessionID      string `json:"session_id"`
	AssistantSeq   int64  `json:"assistant_seq"`
	Status         string `json:"status"`
	ResumeID       string `json:"resume_id"`
	ResumeRejected bool   `json:"resume_rejected"`
	DurationMS     int64  `json:"duration_ms"`
	Error          string `json:"error"`
}

// ---------- F09 chat.error（适配器/平台 → 浏览器） ----------

// ChatErrorPayload 的 Code 与 REST 包络共用 pkg/protocol/errors.go 常量。
type ChatErrorPayload struct {
	AgentID      string `json:"agent_id"`
	SessionID    string `json:"session_id"`
	AssistantSeq int64  `json:"assistant_seq"`
	Code         int    `json:"code"`
	Message      string `json:"message"`
}

// ---------- F10 chat.stop（浏览器 → 平台 → 适配器） ----------

// ChatStopPayload 请求停止当前生成。
type ChatStopPayload struct {
	AgentID   string `json:"agent_id"`
	SessionID string `json:"session_id"`
}

// ---------- F11 agent.status（平台 → 浏览器） ----------

// AgentStatusItem 是单条 agent 的实时三态子集。
type AgentStatusItem struct {
	AgentID    string `json:"agent_id"`
	Online     bool   `json:"online"`
	Registered bool   `json:"registered"`
}

// AgentStatusPayload 广播设备上下线 / 注册态变化。
type AgentStatusPayload struct {
	Agents []AgentStatusItem `json:"agents"`
	Reason string            `json:"reason"`
}

// ---------- F12 / F13 注册态通知（平台 → 适配器） ----------

// AgentUnregisteredPayload 通知适配器清理内存 session id 与在跑的子进程（D-6）。
type AgentUnregisteredPayload struct {
	AgentID string `json:"agent_id"`
}

// AgentRegisteredPayload 是可选的注册成功通知（P1，适配器可预热）。
type AgentRegisteredPayload struct {
	AgentID string `json:"agent_id"`
}

// ---------- F14 client.ready（平台 → 浏览器） ----------

// ClientReadyPayload 是浏览器 WS 建连后的首帧。
type ClientReadyPayload struct {
	ServerTime      string `json:"server_time"`
	ProtocolVersion int    `json:"protocol_version"`
}

// ---------- F15 device.status（平台 → 浏览器） ----------

// DeviceStatusPayload 是设备级上下线推送（P1）。
type DeviceStatusPayload struct {
	DeviceID   string `json:"device_id"`
	Online     bool   `json:"online"`
	LastSeenAt string `json:"last_seen_at"`
}
