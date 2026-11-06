import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react';

import { api } from '../api/client';
import {
  errorMessage,
  FRAME,
  type ChatChunkEvent,
  type ChatChunkPayload,
  type ChatDoneEvent,
  type ChatDonePayload,
  type ChatErrorPayload,
  type ClientReadyPayload,
  type ConnectionState,
  type FrameType,
  type WsFrame,
} from '../api/types';
import { useAuth } from '../store/AuthContext';

/**
 * 全应用唯一一条 /api/v1/ws/client 连接（§10.3 / §10.4）。
 *
 * Seq 三层职责：
 *  L1 连接级 Message.seq —— lastSeqRef 去重，丢弃 seq <= lastSeq 的重复/乱序帧，重连重置；
 *  L2 会话级 messages.seq —— 由平台分配，前端不参与分配；重连成功后由 ChatPage 用
 *     GET /messages?since_seq=<本地最大 L2 seq> 补齐（reconnectEpoch 触发）；
 *  L3 chunk_index —— partsRef 按 index 存增量（重复覆盖、渲染时排序拼接），done/error 时 flush。
 */

const MIN_BACKOFF_MS = 1000;
const MAX_BACKOFF_MS = 30000;
const JITTER_RATIO = 0.2;
const STABLE_CONNECTION_MS = 10000;
const PART_TTL_MS = 10 * 60 * 1000;

type Handler = (event: unknown) => void;

interface StreamParts {
  parts: Map<number, string>;
  agentId: string;
  sessionId: string;
  updatedAt: number;
}

interface ClientWSContextValue {
  connectionState: ConnectionState;
  /** F11 / F15 状态广播计数，页面据此局部刷新。 */
  statusEpoch: number;
  /** 每次「重连成功」+1，ChatPage 据此做 since_seq 补齐。 */
  reconnectEpoch: number;
  subscribe: <T>(type: FrameType, handler: (event: T) => void) => () => void;
  /** 发送上行帧，内部补连接级 seq；未连接返回 false（不排队）。 */
  send: (type: FrameType, payload: object) => boolean;
  reconnectNow: () => void;
}

const ClientWSContext = createContext<ClientWSContextValue | null>(null);

/** 按 chunk_index 升序拼接；重复 index 已在 Map 中覆盖，缺口自然跳过，保证顺序稳定。 */
function joinParts(parts: Map<number, string>): string {
  return Array.from(parts.keys())
    .sort((left, right) => left - right)
    .map((index) => parts.get(index) ?? '')
    .join('');
}

function partsKey(sessionId: string, assistantSeq: number): string {
  return `${sessionId}:${assistantSeq}`;
}

export function ClientWSProvider({ children }: { children: ReactNode }) {
  const { token } = useAuth();

  const [connectionState, setConnectionState] = useState<ConnectionState>('disconnected');
  const [statusEpoch, setStatusEpoch] = useState(0);
  const [reconnectEpoch, setReconnectEpoch] = useState(0);
  const [manualEpoch, setManualEpoch] = useState(0);

  const socketRef = useRef<WebSocket | null>(null);
  const stateRef = useRef<ConnectionState>('disconnected');
  const handlersRef = useRef<Map<FrameType, Set<Handler>>>(new Map());
  const partsRef = useRef<Map<string, StreamParts>>(new Map());
  const sendSeqRef = useRef(0);
  const lastSeqRef = useRef(0);
  const backoffRef = useRef(MIN_BACKOFF_MS);
  const reconnectTimerRef = useRef<number | null>(null);
  const connectedAtRef = useRef(0);
  const readyRef = useRef(false);
  const successfulConnectionsRef = useRef(0);

  const updateState = useCallback((next: ConnectionState) => {
    stateRef.current = next;
    setConnectionState(next);
  }, []);

  const dispatch = useCallback((type: FrameType, event: unknown) => {
    const handlers = handlersRef.current.get(type);
    if (!handlers) return;
    handlers.forEach((handler) => {
      try {
        handler(event);
      } catch (error) {
        // 不打印帧内容（可能含 prompt / token），只记录异常对象。
        console.error('[ClientWS] 帧订阅处理异常', error);
      }
    });
  }, []);

  const subscribe = useCallback(<T,>(type: FrameType, handler: (event: T) => void) => {
    const wrapped = handler as unknown as Handler;
    const existing = handlersRef.current.get(type) ?? new Set<Handler>();
    existing.add(wrapped);
    handlersRef.current.set(type, existing);
    return () => {
      existing.delete(wrapped);
      if (existing.size === 0) handlersRef.current.delete(type);
    };
  }, []);

  const send = useCallback((type: FrameType, payload: object): boolean => {
    const socket = socketRef.current;
    if (!socket || socket.readyState !== WebSocket.OPEN || stateRef.current !== 'connected') {
      return false;
    }
    const seq = (sendSeqRef.current += 1);
    const frame: WsFrame<object> = { type, seq, payload };
    try {
      socket.send(JSON.stringify(frame));
      return true;
    } catch {
      return false;
    }
  }, []);

  const reconnectNow = useCallback(() => {
    backoffRef.current = MIN_BACKOFF_MS;
    setManualEpoch((value) => value + 1);
  }, []);

  useEffect(() => {
    if (!token) {
      updateState('disconnected');
      return undefined;
    }

    let disposed = false;
    let socket: WebSocket | null = null;

    const clearReconnectTimer = () => {
      if (reconnectTimerRef.current !== null) {
        window.clearTimeout(reconnectTimerRef.current);
        reconnectTimerRef.current = null;
      }
    };

    const dispatchFrame = (frame: WsFrame) => {
      const payload = frame.payload;
      switch (frame.type) {
        case FRAME.CLIENT_READY: {
          const wasReconnect = successfulConnectionsRef.current > 0;
          successfulConnectionsRef.current += 1;
          readyRef.current = true;
          backoffRef.current = MIN_BACKOFF_MS;
          updateState('connected');
          dispatch(FRAME.CLIENT_READY, payload as ClientReadyPayload);
          if (wasReconnect) {
            // 重连成功：通知 ChatPage 按 since_seq 补齐已落库消息（R-16）。
            setReconnectEpoch((value) => value + 1);
          }
          break;
        }
        case FRAME.CHAT_SEND_ACK:
          dispatch(FRAME.CHAT_SEND_ACK, payload);
          break;
        case FRAME.CHAT_CHUNK: {
          const chunk = payload as ChatChunkPayload;
          const key = partsKey(chunk.session_id, chunk.assistant_seq);
          let entry = partsRef.current.get(key);
          if (!entry) {
            entry = {
              parts: new Map(),
              agentId: chunk.agent_id,
              sessionId: chunk.session_id,
              updatedAt: Date.now(),
            };
            partsRef.current.set(key, entry);
          }
          entry.updatedAt = Date.now();
          // 重复 chunk_index 幂等覆盖；拼接时按 index 排序，乱序到达也能正确归位。
          entry.parts.set(chunk.chunk_index, chunk.delta);
          const event: ChatChunkEvent = { payload: chunk, content: joinParts(entry.parts) };
          dispatch(FRAME.CHAT_CHUNK, event);
          break;
        }
        case FRAME.CHAT_DONE: {
          const done = payload as ChatDonePayload;
          const key = partsKey(done.session_id, done.assistant_seq);
          const entry = partsRef.current.get(key);
          const event: ChatDoneEvent = {
            payload: done,
            content: entry ? joinParts(entry.parts) : '',
          };
          partsRef.current.delete(key);
          dispatch(FRAME.CHAT_DONE, event);
          break;
        }
        case FRAME.CHAT_ERROR: {
          const failure = payload as ChatErrorPayload;
          partsRef.current.delete(partsKey(failure.session_id, failure.assistant_seq));
          dispatch(FRAME.CHAT_ERROR, {
            ...failure,
            message: failure.message || errorMessage(failure.code),
          } satisfies ChatErrorPayload);
          break;
        }
        case FRAME.AGENT_STATUS:
          setStatusEpoch((value) => value + 1);
          dispatch(FRAME.AGENT_STATUS, payload);
          break;
        case FRAME.DEVICE_STATUS:
          setStatusEpoch((value) => value + 1);
          dispatch(FRAME.DEVICE_STATUS, payload);
          break;
        default:
          // F01~F04 / F12 / F13 属于 adapter 通道，浏览器不应收到；忽略。
          break;
      }
    };

    const handleMessage = (raw: string) => {
      let frame: WsFrame;
      try {
        frame = JSON.parse(raw) as WsFrame;
      } catch {
        return;
      }
      if (!frame || typeof frame.type !== 'string' || typeof frame.seq !== 'number') return;

      // L1：连接级 seq 去重 / 排序；重连时 lastSeqRef 归零。
      if (frame.seq > 0) {
        if (frame.seq <= lastSeqRef.current) return;
        lastSeqRef.current = frame.seq;
      }
      dispatchFrame(frame);
    };

    const scheduleReconnect = () => {
      if (disposed) return;
      const aliveFor = Date.now() - connectedAtRef.current;
      if (readyRef.current && aliveFor >= STABLE_CONNECTION_MS) {
        backoffRef.current = MIN_BACKOFF_MS; // 连接存活 ≥10s 则退避重置
      }
      const base = backoffRef.current;
      const jitter = base * JITTER_RATIO * (Math.random() * 2 - 1);
      const delay = Math.min(MAX_BACKOFF_MS, Math.max(MIN_BACKOFF_MS, base + jitter));
      backoffRef.current = Math.min(MAX_BACKOFF_MS, base * 2);
      updateState('disconnected');

      reconnectTimerRef.current = window.setTimeout(() => {
        reconnectTimerRef.current = null;
        if (disposed) return;
        // 浏览器 WS 拿不到 HTTP 401 状态码；握手从未成功时用 /auth/me 探测登录态，
        // 若 token 失效由 client.ts 统一清 token 并跳登录，避免无限重连。
        if (!readyRef.current) {
          void api.me().catch(() => undefined);
        }
        connect();
      }, delay);
    };

    const connect = () => {
      if (disposed) return;
      clearReconnectTimer();
      sendSeqRef.current = 0;
      lastSeqRef.current = 0;
      readyRef.current = false;
      updateState('connecting');

      // 清理过期流式缓冲（done 丢失时的兜底），保留近期的以支持断线续拼。
      const now = Date.now();
      partsRef.current.forEach((entry, key) => {
        if (now - entry.updatedAt > PART_TTL_MS) partsRef.current.delete(key);
      });

      const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
      const url = `${scheme}//${window.location.host}/api/v1/ws/client?token=${encodeURIComponent(token)}`;
      try {
        // 浏览器 WebSocket 不能自定义 header，鉴权只能走 query（后端已按此实现）。
        socket = new WebSocket(url);
      } catch {
        scheduleReconnect();
        return;
      }
      socketRef.current = socket;
      connectedAtRef.current = Date.now();

      socket.onmessage = (event: MessageEvent) => {
        if (typeof event.data === 'string') handleMessage(event.data);
      };
      socket.onerror = () => {
        // 具体原因由 onclose 统一处理。
      };
      socket.onclose = () => {
        if (disposed) return;
        socketRef.current = null;
        scheduleReconnect();
      };
    };

    const onVisibilityChange = () => {
      if (document.visibilityState !== 'visible' || disposed) return;
      if (socketRef.current === null) {
        clearReconnectTimer();
        connect();
      }
    };
    document.addEventListener('visibilitychange', onVisibilityChange);

    connect();

    return () => {
      disposed = true;
      clearReconnectTimer();
      document.removeEventListener('visibilitychange', onVisibilityChange);
      if (socket) {
        socket.onmessage = null;
        socket.onerror = null;
        socket.onclose = null;
        socket.close(1000, 'client teardown');
      }
      socketRef.current = null;
    };
  }, [token, manualEpoch, dispatch, updateState]);

  const value = useMemo<ClientWSContextValue>(
    () => ({
      connectionState,
      statusEpoch,
      reconnectEpoch,
      subscribe,
      send,
      reconnectNow,
    }),
    [connectionState, statusEpoch, reconnectEpoch, subscribe, send, reconnectNow],
  );

  return <ClientWSContext.Provider value={value}>{children}</ClientWSContext.Provider>;
}

export function useClientWS(): ClientWSContextValue {
  const context = useContext(ClientWSContext);
  if (!context) {
    throw new Error('useClientWS 必须在 ClientWSProvider 内使用');
  }
  return context;
}
