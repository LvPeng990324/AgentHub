/**
 * AgentHub 前端契约镜像（K14 单一事实来源的前端一侧）。
 *
 * REST DTO 逐字段镜像 `internal/server/dto.go` 的 json tag 与 `docs/openapi.yaml` 的 schemas；
 * WS 帧名/错误码/枚举镜像 `pkg/protocol/message.go`、`frames.go`、`errors.go`。
 * 字段名一律 snake_case，禁止同义字段两种拼写；任何帧名/错误码不得散落字符串字面量。
 */

// ---------------------------------------------------------------------------
// REST · 统一包络与 DTO（与 dto.go 逐字一致）
// ---------------------------------------------------------------------------

export interface Envelope<T> {
  code: number;
  message: string;
  data: T | null;
}

export interface LoginRequest {
  password: string;
}

export interface LoginData {
  token: string;
  token_type: string;
  expires_in: number;
}

export interface MeData {
  authenticated: boolean;
  expires_at: string;
}

export interface AgentTypeInfo {
  type: string;
  display_name: string;
  family: string;
}

export interface MetaData {
  name: string;
  version: string;
  domain: string;
  protocol_version: number;
  agent_types: AgentTypeInfo[];
}

export interface HealthData {
  status: string;
  uptime_s: number;
  db: string;
}

/** 设备页展开项里的 agent 摘要（Q14 只读 version / exec_path）。 */
export interface AgentBriefDTO {
  id: string;
  type: string;
  display_name: string;
  version: string;
  exec_path: string;
  registered: boolean;
  online: boolean;
}

export interface DeviceDTO {
  id: string;
  name: string;
  os: string;
  arch: string;
  adapter_version: string;
  protocol_version: number;
  online: boolean;
  last_seen_at: string | null;
  remark: string;
  created_at: string;
  agents: AgentBriefDTO[];
}

export interface DeviceListData {
  items: DeviceDTO[];
  total: number;
}

export interface RemarkRequest {
  remark: string;
}

export interface AgentDTO {
  id: string;
  device_id: string;
  device_name: string;
  type: string;
  display_name: string;
  name: string;
  version: string;
  exec_path: string;
  remark: string;
  registered: boolean;
  online: boolean;
  last_seen_at: string | null;
  created_at: string;
  session_id: string | null;
}

export interface AgentListData {
  items: AgentDTO[];
  total: number;
}

export interface BatchRegisterRequest {
  agent_ids: string[];
}

export interface BatchRegisterData {
  registered: string[];
  skipped: string[];
}

export interface UnregisterData {
  agent_id: string;
  registered: boolean;
  messages_cleared: boolean;
}

export interface MsgDTO {
  seq: number;
  role: string;
  content: string;
  status: string;
  created_at: string;
}

export interface MessageListData {
  items: MsgDTO[];
  has_more: boolean;
  latest_seq: number;
}

export interface SessionData {
  session_id: string | null;
  resume_id: string;
  message_count: number;
}

/** 列表端点查询参数，与 dto.go:ParseListQuery 一致（limit 默认 100 / 上限 500）。 */
export interface ListParams {
  q?: string;
  type?: string;
  device_id?: string;
  online?: boolean;
  registered?: boolean;
  limit?: number;
  offset?: number;
}

export const DEFAULT_LIST_LIMIT = 100;
export const MAX_LIST_LIMIT = 500;
export const MAX_REMARK_LENGTH = 500;
export const MAX_PROMPT_BYTES = 64 * 1024;

// ---------------------------------------------------------------------------
// WS · F01~F15 帧名（镜像 pkg/protocol/message.go，唯一事实来源）
// ---------------------------------------------------------------------------

export const FRAME = {
  DEVICE_REGISTER: 'device.register', // F01 adapter → 平台（前端不消费）
  DEVICE_REGISTER_ACK: 'device.register_ack', // F02 平台 → adapter（前端不消费）
  DEVICE_HEARTBEAT: 'device.heartbeat', // F03 adapter → 平台（前端不消费）
  HEARTBEAT_ACK: 'heartbeat_ack', // F04 平台 → adapter（前端不消费）
  CHAT_SEND: 'chat.send', // F05 浏览器上行
  CHAT_SEND_ACK: 'chat.send_ack', // F06
  CHAT_CHUNK: 'chat.chunk', // F07
  CHAT_DONE: 'chat.done', // F08
  CHAT_ERROR: 'chat.error', // F09
  CHAT_STOP: 'chat.stop', // F10 浏览器上行
  AGENT_STATUS: 'agent.status', // F11
  AGENT_UNREGISTERED: 'agent.unregistered', // F12 平台 → adapter（前端不消费）
  AGENT_REGISTERED: 'agent.registered', // F13 平台 → adapter（前端不消费）
  CLIENT_READY: 'client.ready', // F14
  DEVICE_STATUS: 'device.status', // F15
} as const;

export type FrameType = (typeof FRAME)[keyof typeof FRAME];

/** 帧信封：seq 为连接级序号（L1），重连重置为 1。 */
export interface WsFrame<T = unknown> {
  type: FrameType;
  seq: number;
  payload: T;
}

// ---------------------------------------------------------------------------
// WS · payload 镜像（pkg/protocol/frames.go）
// ---------------------------------------------------------------------------

export interface ClientReadyPayload {
  server_time: string;
  protocol_version: number;
}

/**
 * 平台补齐字段 session_id / user_seq / assistant_seq 在 frames.go 标 omitempty，
 * 故在前端为可选；resume_id 无 omitempty（空串 = 首轮）。
 * 浏览器上行**绝不能**伪造这三个平台补齐字段，见 ChatSendClientPayload。
 */
export interface ChatSendPayload {
  agent_id: string;
  content: string;
  session_id?: string;
  user_seq?: number;
  assistant_seq?: number;
  resume_id: string;
}

/** 浏览器 → 平台的 chat.send payload：只有 agent_id / content。 */
export type ChatSendClientPayload = Pick<ChatSendPayload, 'agent_id' | 'content'>;

export interface ChatSendAckPayload {
  agent_id: string;
  session_id: string;
  user_seq: number;
  assistant_seq: number;
  created_at: string;
}

export interface ChatChunkPayload {
  agent_id: string;
  session_id: string;
  assistant_seq: number;
  chunk_index: number;
  delta: string;
}

export interface ChatDonePayload {
  agent_id: string;
  session_id: string;
  assistant_seq: number;
  status: string;
  resume_id: string;
  resume_rejected: boolean;
  duration_ms: number;
  error: string;
}

export interface ChatErrorPayload {
  agent_id: string;
  session_id: string;
  assistant_seq: number;
  code: number;
  message: string;
}

export interface ChatStopPayload {
  agent_id: string;
  session_id: string;
}

export interface AgentStatusItem {
  agent_id: string;
  online: boolean;
  registered: boolean;
}

export interface AgentStatusPayload {
  agents: AgentStatusItem[];
  reason: string;
}

export interface DeviceStatusPayload {
  device_id: string;
  online: boolean;
  last_seen_at: string;
}

/** ClientWS 对 chat.chunk 派发的事件（带会话级 seq 与 chunk_index）。 */
export interface ChatChunkEvent {
  payload: ChatChunkPayload;
  content: string;
}

/** ClientWS 对 chat.done 派发的事件：content 是按 chunk_index 拼好的完整文本。 */
export interface ChatDoneEvent {
  payload: ChatDonePayload;
  content: string;
}

export type ConnectionState = 'connecting' | 'connected' | 'disconnected' | 'unauthorized';

// ---------------------------------------------------------------------------
// 共享枚举（镜像 pkg/protocol/frames.go）
// ---------------------------------------------------------------------------

export const ROLE = {
  USER: 'user',
  ASSISTANT: 'assistant',
  SYSTEM: 'system',
} as const;
export type Role = (typeof ROLE)[keyof typeof ROLE];

export const MESSAGE_STATUS = {
  STREAMING: 'streaming',
  COMPLETE: 'complete',
  ERROR: 'error',
  STOPPED: 'stopped',
  INTERRUPTED: 'interrupted',
} as const;
export type MessageStatus = (typeof MESSAGE_STATUS)[keyof typeof MESSAGE_STATUS];

/** chat.done 的 status 与 messages.status 拼写不同，禁止混用。 */
export const DONE_STATUS = {
  COMPLETED: 'completed',
  STOPPED: 'stopped',
  INTERRUPTED: 'interrupted',
  FAILED: 'failed',
} as const;
export type DoneStatus = (typeof DONE_STATUS)[keyof typeof DONE_STATUS];

export const STATUS_REASON = {
  DEVICE_ONLINE: 'device_online',
  DEVICE_OFFLINE: 'device_offline',
  REGISTERED: 'registered',
  UNREGISTERED: 'unregistered',
  SNAPSHOT: 'snapshot',
  STALE_STREAMING: 'stale_streaming',
  ADAPTER_RECONNECT: 'adapter_reconnect',
} as const;

export const PROTOCOL_FAMILY = {
  STREAM: 'stream',
  ACP: 'acp',
  APP_SERVER: 'app-server',
  JSON: 'json',
} as const;

/** 9 种 Agent 类型（§8.3 / /meta agent_types），用于筛选芯片与彩色徽标。 */
export const AGENT_TYPE = {
  CLAUDE: 'claude',
  CODEBUDDY: 'codebuddy',
  QWEN: 'qwen',
  CURSOR: 'cursor',
  HERMES: 'hermes',
  KIMI: 'kimi',
  CODEX: 'codex',
  OPENCODE: 'opencode',
  OPENCLAW: 'openclaw',
} as const;

export const AGENT_TYPE_LABELS: Record<string, string> = {
  [AGENT_TYPE.CLAUDE]: 'Claude Code',
  [AGENT_TYPE.CODEBUDDY]: 'CodeBuddy',
  [AGENT_TYPE.QWEN]: 'Qwen Code',
  [AGENT_TYPE.CURSOR]: 'Cursor Agent',
  [AGENT_TYPE.HERMES]: 'Hermes',
  [AGENT_TYPE.KIMI]: 'Kimi Code',
  [AGENT_TYPE.CODEX]: 'Codex',
  [AGENT_TYPE.OPENCODE]: 'OpenCode',
  [AGENT_TYPE.OPENCLAW]: 'OpenClaw',
};

// ---------------------------------------------------------------------------
// 错误码（镜像 pkg/protocol/errors.go，REST 包络与 chat.error.code 共用）
// ---------------------------------------------------------------------------

export const CODE = {
  OK: 0,
  UNAUTHORIZED: 1001,
  INVALID_PASSWORD: 1002,
  TOKEN_EXPIRED: 1003,
  BAD_REQUEST: 1004,
  NOT_FOUND: 1005,
  DEVICE_NOT_FOUND: 2001,
  AGENT_NOT_FOUND: 3001,
  AGENT_NOT_REGISTERED: 3002,
  AGENT_OFFLINE: 3003,
  SESSION_NOT_FOUND: 4001,
  MESSAGE_TOO_LARGE: 4002,
  GENERATION_IN_PROGRESS: 4003,
  INTERNAL_ERROR: 5000,
  ADAPTER_UNAUTHORIZED: 5001,
  PROTOCOL_VERSION_MISMATCH: 5002,
  CLI_START_FAILED: 5003,
  RESUME_REJECTED: 5004,
  GENERATION_TIMEOUT: 5005,
} as const;

export type ErrorCode = (typeof CODE)[keyof typeof CODE];

/** 中文文案镜像 errors.go:codeRegistry；未知码回退内部错误文案。 */
export const ERROR_MESSAGE: Record<number, string> = {
  [CODE.OK]: 'ok',
  [CODE.UNAUTHORIZED]: '请先登录',
  [CODE.INVALID_PASSWORD]: '密码错误',
  [CODE.TOKEN_EXPIRED]: '登录已过期，请重新登录',
  [CODE.BAD_REQUEST]: '请求参数有误',
  [CODE.NOT_FOUND]: '请求的资源不存在',
  [CODE.DEVICE_NOT_FOUND]: '设备不存在',
  [CODE.AGENT_NOT_FOUND]: 'Agent 不存在',
  [CODE.AGENT_NOT_REGISTERED]: '该 Agent 尚未注册',
  [CODE.AGENT_OFFLINE]: 'Agent 当前离线，暂时无法发送消息',
  [CODE.SESSION_NOT_FOUND]: '会话不存在',
  [CODE.MESSAGE_TOO_LARGE]: '消息内容过长',
  [CODE.GENERATION_IN_PROGRESS]: '上一条回复仍在生成中',
  [CODE.INTERNAL_ERROR]: '服务内部错误，请稍后重试',
  [CODE.ADAPTER_UNAUTHORIZED]: '设备令牌无效',
  [CODE.PROTOCOL_VERSION_MISMATCH]: '适配器版本不兼容，请升级',
  [CODE.CLI_START_FAILED]: 'Agent 启动失败，请检查本机 CLI',
  [CODE.RESUME_REJECTED]: '会话恢复失败，已开启新会话',
  [CODE.GENERATION_TIMEOUT]: '回复超时，已停止',
};

export function errorMessage(code: number): string {
  return ERROR_MESSAGE[code] ?? ERROR_MESSAGE[CODE.INTERNAL_ERROR];
}

/** chat.done 终态 → 落库 messages.status 的映射（与后端 mapDoneStatus 一致）。 */
export function doneStatusToMessageStatus(doneStatus: string): MessageStatus {
  switch (doneStatus) {
    case DONE_STATUS.COMPLETED:
      return MESSAGE_STATUS.COMPLETE;
    case DONE_STATUS.STOPPED:
      return MESSAGE_STATUS.STOPPED;
    case DONE_STATUS.INTERRUPTED:
      return MESSAGE_STATUS.INTERRUPTED;
    case DONE_STATUS.FAILED:
      return MESSAGE_STATUS.ERROR;
    default:
      return MESSAGE_STATUS.COMPLETE;
  }
}
