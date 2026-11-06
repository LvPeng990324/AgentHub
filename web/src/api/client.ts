/**
 * 统一 fetch 封装（§10.1 / R-01 / R-18）。
 *
 * - 解析统一包络 {code,message,data}；code!==0 抛带中文 message 的 ApiError；
 * - 自动注入 Authorization: Bearer <token>；401 响应或 UNAUTHORIZED / TOKEN_EXPIRED 错误码清 token 并触发跳登录；
 * - 路径与请求体严格对照 docs/openapi.yaml 的 14 个 paths。
 */

import {
  CODE,
  DEFAULT_LIST_LIMIT,
  MAX_LIST_LIMIT,
  type AgentDTO,
  type AgentListData,
  type BatchRegisterData,
  type DeviceDTO,
  type DeviceListData,
  type Envelope,
  type HealthData,
  type ListParams,
  type LoginData,
  type MeData,
  type MessageListData,
  type MetaData,
  type SessionData,
  type UnregisterData,
} from './types';

const API_BASE = '/api/v1';
const TOKEN_STORAGE_KEY = 'agenthub.token';

/** 业务错误：message 为可直接展示的简体中文。 */
export class ApiError extends Error {
  readonly code: number;
  readonly status: number;

  constructor(code: number, message: string, status: number) {
    super(message);
    this.name = 'ApiError';
    this.code = code;
    this.status = status;
  }
}

export function getToken(): string | null {
  try {
    return window.localStorage.getItem(TOKEN_STORAGE_KEY);
  } catch {
    return null;
  }
}

export function setToken(token: string): void {
  try {
    window.localStorage.setItem(TOKEN_STORAGE_KEY, token);
  } catch {
    // 隐私模式下 localStorage 可能不可写，此时仅保留内存态。
  }
}

export function clearToken(): void {
  try {
    window.localStorage.removeItem(TOKEN_STORAGE_KEY);
  } catch {
    // 同上，忽略存储异常。
  }
}

let unauthorizedHandler: (() => void) | null = null;

/** AuthProvider 注册 401 回调（清 token + 触发 ProtectedRoute 跳转）。 */
export function setUnauthorizedHandler(handler: (() => void) | null): void {
  unauthorizedHandler = handler;
}

type HttpMethod = 'GET' | 'POST' | 'PATCH' | 'DELETE';

interface RequestOptions {
  method?: HttpMethod;
  body?: unknown;
  signal?: AbortSignal;
  /** meta / login / health 为公开端点，不需要 token。 */
  public?: boolean;
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json';
  }
  if (!options.public) {
    const token = getToken();
    if (token) {
      headers.Authorization = `Bearer ${token}`;
    }
  }

  let response: Response;
  try {
    response = await fetch(`${API_BASE}${path}`, {
      method: options.method ?? 'GET',
      headers,
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
      signal: options.signal,
    });
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') {
      throw error;
    }
    throw new ApiError(CODE.INTERNAL_ERROR, '无法连接平台服务，请检查网络', 0);
  }

  let envelope: Envelope<T>;
  try {
    envelope = (await response.json()) as Envelope<T>;
  } catch {
    throw new ApiError(CODE.INTERNAL_ERROR, '服务响应异常，请稍后重试', response.status);
  }

  const code = typeof envelope?.code === 'number' ? envelope.code : CODE.INTERNAL_ERROR;
  const message = envelope?.message || '服务响应异常，请稍后重试';

  // 登录态失效：即时清 token，并通知 AuthProvider（ProtectedRoute 会跳登录）。
  if (
    response.status === 401 ||
    code === CODE.UNAUTHORIZED ||
    code === CODE.TOKEN_EXPIRED
  ) {
    clearToken();
    unauthorizedHandler?.();
    throw new ApiError(code, message || '请先登录', response.status);
  }

  if (code !== CODE.OK || !response.ok || envelope?.data === null) {
    // data 为 null 的成功响应（如 logout）由调用方用 null 类型声明，不视为错误。
    if (code === CODE.OK && response.ok) {
      return envelope.data as T;
    }
    throw new ApiError(code, message, response.status);
  }
  return envelope.data as T;
}

/** 构造列表查询串；undefined / 空串不参与，布尔值转 "true"/"false"。 */
export function buildQuery(params: ListParams = {}): string {
  const search = new URLSearchParams();
  if (params.q) search.set('q', params.q);
  if (params.type) search.set('type', params.type);
  if (params.device_id) search.set('device_id', params.device_id);
  if (params.online !== undefined) search.set('online', String(params.online));
  if (params.registered !== undefined) search.set('registered', String(params.registered));
  if (params.limit !== undefined) {
    search.set('limit', String(Math.min(Math.max(params.limit, 1), MAX_LIST_LIMIT)));
  }
  if (params.offset !== undefined && params.offset >= 0) {
    search.set('offset', String(params.offset));
  }
  const query = search.toString();
  return query ? `?${query}` : '';
}

function encodeId(id: string): string {
  // agent_id 含 ':'，必须 encodeURIComponent（Go r.PathValue 会自动解码）。
  return encodeURIComponent(id);
}

/** 14 个 REST 端点（与 docs/openapi.yaml 一一对应）。 */
export const api = {
  // E04 / E05：公开
  getMeta: () => request<MetaData>('/meta', { public: true }),
  getHealth: () => request<HealthData>('/health', { public: true }),

  // E01 / E02 / E03
  login: (password: string) =>
    request<LoginData>('/auth/login', { method: 'POST', body: { password }, public: true }),
  logout: () => request<null>('/auth/logout', { method: 'POST' }),
  me: () => request<MeData>('/auth/me'),

  // E06 / E07
  listDevices: (params: ListParams = {}) =>
    request<DeviceListData>(`/devices${buildQuery(params)}`),
  updateDeviceRemark: (deviceId: string, remark: string) =>
    request<DeviceDTO>(`/devices/${encodeId(deviceId)}/remark`, {
      method: 'PATCH',
      body: { remark },
    }),

  // E08 / E09
  listDiscovered: (params: ListParams = {}) =>
    request<AgentListData>(`/agents/discovered${buildQuery(params)}`),
  listAgents: (params: ListParams = {}) =>
    request<AgentListData>(`/agents${buildQuery(params)}`),

  // E10 / E11 / E12 / E13
  registerAgent: (agentId: string) =>
    request<AgentDTO>(`/agents/${encodeId(agentId)}/register`, { method: 'POST' }),
  unregisterAgent: (agentId: string) =>
    request<UnregisterData>(`/agents/${encodeId(agentId)}/register`, { method: 'DELETE' }),
  batchRegister: (agentIds: string[]) =>
    request<BatchRegisterData>('/agents/batch-register', {
      method: 'POST',
      body: { agent_ids: agentIds },
    }),
  updateAgentRemark: (agentId: string, remark: string) =>
    request<AgentDTO>(`/agents/${encodeId(agentId)}/remark`, {
      method: 'PATCH',
      body: { remark },
    }),

  // E14 / E15
  getMessages: (agentId: string, sinceSeq = 0, limit = DEFAULT_LIST_LIMIT) =>
    request<MessageListData>(
      `/agents/${encodeId(agentId)}/messages?since_seq=${Math.max(0, Math.floor(sinceSeq))}&limit=${Math.min(
        Math.max(limit, 1),
        MAX_LIST_LIMIT,
      )}`,
    ),
  getSession: (agentId: string) =>
    request<SessionData>(`/agents/${encodeId(agentId)}/session`),
};

/** 把任意异常转为可展示的中文文案。 */
export function toErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  if (error instanceof Error && error.message) return error.message;
  return '服务内部错误，请稍后重试';
}

/** 把 RFC3339 UTC 时间转为中文相对时间（设备最后心跳展示；各页共用，避免重复实现）。 */
export function formatRelativeTime(iso: string | null): string {
  if (!iso) return '暂无心跳';
  const timestamp = Date.parse(iso);
  if (Number.isNaN(timestamp)) return '时间未知';
  const seconds = Math.max(0, Math.floor((Date.now() - timestamp) / 1000));
  if (seconds < 60) return `${seconds} 秒前`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟前`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时前`;
  return `${Math.floor(seconds / 86400)} 天前`;
}
