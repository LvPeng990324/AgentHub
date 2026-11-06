import clsx from 'clsx';
import { ArrowLeft, Send, Square } from 'lucide-react';
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type KeyboardEvent,
} from 'react';
import { useNavigate, useParams } from 'react-router-dom';

import { api, toErrorMessage } from '../api/client';
import {
  CODE,
  errorMessage,
  FRAME,
  MAX_LIST_LIMIT,
  MAX_PROMPT_BYTES,
  MESSAGE_STATUS,
  ROLE,
  doneStatusToMessageStatus,
  type AgentDTO,
  type AgentStatusPayload,
  type ChatChunkEvent,
  type ChatDoneEvent,
  type ChatErrorPayload,
  type ChatSendAckPayload,
  type ChatSendClientPayload,
  type ChatStopPayload,
  type MessageStatus,
  type MsgDTO,
} from '../api/types';
import { Banner } from '../components/Banner';
import { EmptyState } from '../components/EmptyState';
import { Spinner } from '../components/Spinner';
import { StatusDot } from '../components/StatusDot';
import { TypeBadge } from '../components/TypeBadge';
import { useClientWS } from '../ws/ClientWS';

/** 对话页局部消息模型：seq 是会话级权威序号（L2），parts 保存 chunk_index 增量（L3）。 */
interface ChatMessage {
  key: string;
  seq: number | null;
  role: string;
  content: string;
  status: MessageStatus;
  created_at: string;
  parts?: Map<number, string>;
  /** CLI 原始错误详情（来自 chat.done.error / chat.error.message），仅用于失败气泡折叠展示。 */
  error?: string;
}

const KNOWN_STATUSES = new Set<string>(Object.values(MESSAGE_STATUS));

function toMessageStatus(raw: string): MessageStatus {
  return KNOWN_STATUSES.has(raw) ? (raw as MessageStatus) : MESSAGE_STATUS.COMPLETE;
}

function toChatMessage(item: MsgDTO): ChatMessage {
  return {
    key: `m-${item.seq}`,
    seq: item.seq,
    role: item.role,
    content: item.content,
    status: toMessageStatus(item.status),
    created_at: item.created_at,
  };
}

function statusLabel(status: MessageStatus): string {
  switch (status) {
    case MESSAGE_STATUS.STOPPED:
      return '已停止生成';
    case MESSAGE_STATUS.INTERRUPTED:
      return '生成已中断';
    case MESSAGE_STATUS.ERROR:
      return '生成失败';
    case MESSAGE_STATUS.STREAMING:
      return '生成中…';
    default:
      return '';
  }
}

/** 详情默认展示的字符数；超出后折叠，点击「展开详情」查看全文。 */
const ERROR_DETAIL_LIMIT = 300;

/**
 * 失败气泡的「详情」行：展示 CLI 原始报错（如 mcpServers Required value is missing），
 * 让用户能自助定位问题。只展示 done.error / chat.error.message，**绝不展示 prompt**。
 * 长串用 break-all + pre-wrap 换行，375px 下不撑破气泡。
 */
function ErrorDetail({ text }: { text: string }) {
  const [expanded, setExpanded] = useState(false);
  const overflows = text.length > ERROR_DETAIL_LIMIT;
  const shown = overflows && !expanded ? `${text.slice(0, ERROR_DETAIL_LIMIT)}…` : text;
  return (
    <div className="mt-1 min-w-0">
      <p className="min-w-0 whitespace-pre-wrap break-all font-mono text-2xs leading-relaxed text-danger opacity-80">
        详情：{shown}
      </p>
      {overflows && (
        <button
          type="button"
          onClick={() => setExpanded((value) => !value)}
          className="mt-1 inline-flex min-h-[44px] items-center text-2xs text-danger underline decoration-dotted underline-offset-2"
        >
          {expanded ? '收起详情' : '展开详情'}
        </button>
      )}
    </div>
  );
}

function draftStorageKey(agentId: string): string {
  return `agenthub.draft.${agentId}`;
}

/**
 * P-5 对话页：进入加载历史 + 会话信息；流式增量逐字渲染；
 * 生成中禁用发送并允许停止；断线重连按 since_seq 补齐；草稿按 agent 隔离。
 */
export function ChatPage() {
  const { agentId = '' } = useParams<{ agentId: string }>();
  const navigate = useNavigate();
  const { connectionState, subscribe, send, reconnectEpoch, statusEpoch } = useClientWS();

  const [agent, setAgent] = useState<AgentDTO | null>(null);
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [metaError, setMetaError] = useState<string | null>(null);
  const [chatError, setChatError] = useState<string | null>(null);
  const [hint, setHint] = useState<string | null>(null);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [loadingHistory, setLoadingHistory] = useState(true);
  const [generating, setGenerating] = useState(false);
  const [draft, setDraft] = useState(() => {
    try {
      return sessionStorage.getItem(draftStorageKey(agentId)) ?? '';
    } catch {
      return '';
    }
  });
  const [draftAgentId, setDraftAgentId] = useState(agentId);

  const latestSeqRef = useRef(0);
  const pendingLocalKeyRef = useRef<string | null>(null);
  const lastPromptRef = useRef('');
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const atBottomRef = useRef(true);

  // 草稿按 agentId 隔离：切换 agent 时读取该 agent 自己的草稿，避免串草稿。
  useEffect(() => {
    if (draftAgentId === agentId) return;
    try {
      setDraft(sessionStorage.getItem(draftStorageKey(agentId)) ?? '');
    } catch {
      setDraft('');
    }
    setDraftAgentId(agentId);
  }, [agentId, draftAgentId]);

  useEffect(() => {
    if (draftAgentId !== agentId) return;
    try {
      sessionStorage.setItem(draftStorageKey(agentId), draft);
    } catch {
      // 隐私模式下忽略存储异常。
    }
  }, [agentId, draft, draftAgentId]);

  // 读取 agent 全量信息与当前会话（单会话 Q4）。
  useEffect(() => {
    let alive = true;
    void (async () => {
      try {
        const [registered, discovered] = await Promise.all([
          api.listAgents({ limit: MAX_LIST_LIMIT }),
          api.listDiscovered({ limit: MAX_LIST_LIMIT }),
        ]);
        if (!alive) return;
        const found =
          registered.items.find((item) => item.id === agentId) ??
          discovered.items.find((item) => item.id === agentId) ??
          null;
        setAgent(found);
        if (found && !found.registered) {
          setMetaError('该 Agent 尚未注册，请先在「发现与管理」中注册');
        } else {
          setMetaError(null);
        }
      } catch (caught) {
        if (alive) setMetaError(toErrorMessage(caught));
      }
      try {
        const session = await api.getSession(agentId);
        if (alive) setSessionId(session.session_id);
      } catch {
        if (alive) setSessionId(null);
      }
    })();
    return () => {
      alive = false;
    };
  }, [agentId, statusEpoch]);

  // 初始历史加载（since_seq=0），并按 L2 seq 排序。
  useEffect(() => {
    let alive = true;
    setLoadingHistory(true);
    setMessages([]);
    latestSeqRef.current = 0;
    void api
      .getMessages(agentId, 0, MAX_LIST_LIMIT)
      .then((data) => {
        if (!alive) return;
        setMessages(data.items.map(toChatMessage));
        latestSeqRef.current = data.latest_seq;
      })
      .catch((caught) => {
        if (alive) setChatError(toErrorMessage(caught));
      })
      .finally(() => {
        if (alive) setLoadingHistory(false);
      });
    return () => {
      alive = false;
    };
  }, [agentId]);

  /** 合并 REST 补齐的消息：按会话级 seq 去重、排序，保留尚未确认的本地临时气泡。 */
  const mergeMessages = useCallback((incoming: MsgDTO[]) => {
    setMessages((previous) => {
      const bySeq = new Map<number, ChatMessage>();
      previous.forEach((message) => {
        if (message.seq !== null) bySeq.set(message.seq, message);
      });
      incoming.forEach((item) => bySeq.set(item.seq, toChatMessage(item)));
      const merged = Array.from(bySeq.values()).sort(
        (left, right) => (left.seq ?? 0) - (right.seq ?? 0),
      );
      const locals = previous.filter((message) => message.seq === null);
      return [...merged, ...locals];
    });
    incoming.forEach((item) => {
      latestSeqRef.current = Math.max(latestSeqRef.current, item.seq);
    });
  }, []);

  // 重连成功后按 since_seq 补齐已落库消息，不重复不丢序（R-16）。
  useEffect(() => {
    if (reconnectEpoch === 0) return;
    void api
      .getMessages(agentId, latestSeqRef.current, MAX_LIST_LIMIT)
      .then((data) => mergeMessages(data.items))
      .catch(() => undefined);
  }, [reconnectEpoch, agentId, mergeMessages]);

  // WS 帧订阅：F06 ack / F07 chunk / F08 done / F09 error / F11 status。
  useEffect(() => {
    const offAck = subscribe<ChatSendAckPayload>(FRAME.CHAT_SEND_ACK, (ack) => {
      if (ack.agent_id !== agentId) return;
      setSessionId(ack.session_id);
      latestSeqRef.current = Math.max(latestSeqRef.current, ack.assistant_seq);
      const localKey = pendingLocalKeyRef.current;
      pendingLocalKeyRef.current = null;
      setMessages((previous) => {
        const next = previous.map((message) =>
          message.key === localKey
            ? { ...message, key: `m-${ack.user_seq}`, seq: ack.user_seq, created_at: ack.created_at }
            : message,
        );
        if (!next.some((message) => message.seq === ack.assistant_seq)) {
          next.push({
            key: `m-${ack.assistant_seq}`,
            seq: ack.assistant_seq,
            role: ROLE.ASSISTANT,
            content: '',
            status: MESSAGE_STATUS.STREAMING,
            created_at: ack.created_at,
          });
        }
        return next.sort(
          (left, right) => (left.seq ?? Number.MAX_SAFE_INTEGER) - (right.seq ?? Number.MAX_SAFE_INTEGER),
        );
      });
      setGenerating(true);
    });

    const offChunk = subscribe<ChatChunkEvent>(FRAME.CHAT_CHUNK, (event) => {
      if (event.payload.agent_id !== agentId) return;
      setMessages((previous) =>
        previous.map((message) => {
          if (message.seq !== event.payload.assistant_seq) return message;
          const parts = new Map(message.parts ?? []);
          // 重复 chunk_index 幂等覆盖；完整文本由 Provider 按序拼接后给出。
          parts.set(event.payload.chunk_index, event.payload.delta);
          return { ...message, parts, content: event.content, status: MESSAGE_STATUS.STREAMING };
        }),
      );
    });

    const offDone = subscribe<ChatDoneEvent>(FRAME.CHAT_DONE, (event) => {
      if (event.payload.agent_id !== agentId) return;
      const status = doneStatusToMessageStatus(event.payload.status);
      setMessages((previous) =>
        previous.map((message) =>
          message.seq === event.payload.assistant_seq
            ? {
                ...message,
                content: event.content || message.content,
                status,
                // 失败时把 CLI 原始报错带进气泡，供「详情」折叠展示（成功态为空则不渲染）。
                error: event.payload.error || message.error,
                parts: undefined,
              }
            : message,
        ),
      );
      setGenerating(false);
      if (event.payload.resume_rejected) {
        setHint(errorMessage(CODE.RESUME_REJECTED));
      }
    });

    const offError = subscribe<ChatErrorPayload>(FRAME.CHAT_ERROR, (failure) => {
      if (failure.agent_id && failure.agent_id !== agentId) return;
      setChatError(failure.message || errorMessage(failure.code));
      setGenerating(false);
      if (failure.assistant_seq > 0) {
        setMessages((previous) =>
          previous.map((message) =>
            message.seq === failure.assistant_seq
              ? {
                  ...message,
                  status: MESSAGE_STATUS.ERROR,
                  error: failure.message || message.error,
                }
              : message,
          ),
        );
      }
    });

    const offStatus = subscribe<AgentStatusPayload>(FRAME.AGENT_STATUS, (payload) => {
      const item = payload.agents.find((entry) => entry.agent_id === agentId);
      if (!item) return;
      setAgent((previous) =>
        previous ? { ...previous, online: item.online, registered: item.registered } : previous,
      );
    });

    return () => {
      offAck();
      offChunk();
      offDone();
      offError();
      offStatus();
    };
  }, [subscribe, agentId]);

  // 自动滚动：仅在用户处于底部时贴底，上滚查看历史不打断（§10.5）。
  useEffect(() => {
    if (!atBottomRef.current) return;
    const element = scrollRef.current;
    if (element) element.scrollTop = element.scrollHeight;
  }, [messages]);

  const onScroll = () => {
    const element = scrollRef.current;
    if (!element) return;
    atBottomRef.current = element.scrollHeight - element.scrollTop - element.clientHeight < 80;
  };

  const registered = agent?.registered ?? false;
  const online = agent?.online ?? false;
  const connected = connectionState === 'connected';
  const canSend =
    registered && online && connected && !generating && draft.trim().length > 0 && !metaError;

  const handleSend = () => {
    const content = draft.trim();
    if (!content || !canSend) return;
    if (new Blob([content]).size > MAX_PROMPT_BYTES) {
      setChatError('消息内容过长，请精简后再发送');
      return;
    }
    // 上行帧只带 agent_id / content，绝不伪造 session_id / user_seq / assistant_seq / resume_id。
    const payload: ChatSendClientPayload = { agent_id: agentId, content };
    if (!send(FRAME.CHAT_SEND, payload)) {
      setChatError('实时连接未就绪，请稍后重试');
      return;
    }
    const localKey = `local-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    pendingLocalKeyRef.current = localKey;
    lastPromptRef.current = content;
    setMessages((previous) => [
      ...previous,
      {
        key: localKey,
        seq: null,
        role: ROLE.USER,
        content,
        status: MESSAGE_STATUS.COMPLETE,
        created_at: new Date().toISOString(),
      },
    ]);
    setDraft('');
    setChatError(null);
    setHint(null);
    setGenerating(true);
    atBottomRef.current = true;
  };

  const handleStop = () => {
    if (!sessionId) return;
    const payload: ChatStopPayload = { agent_id: agentId, session_id: sessionId };
    send(FRAME.CHAT_STOP, payload);
    // 本地立即置 stopped 视觉态，等 chat.done{stopped} 确认（R-14）。
    setMessages((previous) =>
      previous.map((message) =>
        message.status === MESSAGE_STATUS.STREAMING
          ? { ...message, status: MESSAGE_STATUS.STOPPED }
          : message,
      ),
    );
    setGenerating(false);
  };

  const onTextareaKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing) {
      event.preventDefault();
      handleSend();
    }
  };

  return (
    <div className="flex h-full min-h-0 min-w-0 flex-col overflow-x-hidden bg-surface-muted">
      {/* 会话头：移动端吸顶，返回按钮 ≥44px */}
      <header className="sticky top-0 z-10 flex shrink-0 items-center gap-2 border-b border-line bg-surface px-2 py-2">
        <button
          type="button"
          onClick={() => navigate('/agents')}
          className="tap-target flex shrink-0 items-center justify-center rounded-card text-ink"
          aria-label="返回 Agent 列表"
        >
          <ArrowLeft size={20} aria-hidden="true" />
        </button>
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 items-center gap-2">
            <h1 className="min-w-0 truncate text-sm font-semibold text-ink">
              {agent?.name ?? agentId}
            </h1>
            {agent && <StatusDot online={agent.online} />}
          </div>
          <div className="flex min-w-0 items-center gap-2">
            {agent && <TypeBadge type={agent.type} displayName={agent.display_name} />}
            {agent && (
              <span className="min-w-0 truncate text-2xs text-muted">
                {agent.device_name || agent.device_id}
              </span>
            )}
          </div>
        </div>
      </header>

      {/* 状态横幅 */}
      <div className="flex shrink-0 flex-col gap-2 px-3 pt-2">
        {!connected && (
          <Banner variant="offline" title="实时连接已断开">
            正在自动重连，恢复后会自动补齐历史消息；恢复前无法发送消息。
          </Banner>
        )}
        {!registered && !metaError && agent && (
          <Banner variant="error" title="该 Agent 尚未注册">
            请先在「发现与管理」中注册后再发起对话。
          </Banner>
        )}
        {metaError && <Banner variant="error">{metaError}</Banner>}
        {registered && !online && (
          <Banner variant="warning" title="设备离线">
            Agent 当前离线，暂时无法发送消息；恢复连接后自动可用。
          </Banner>
        )}
        {chatError && (
          <Banner
            variant="error"
            title="发送或生成失败"
            action={
              <button
                type="button"
                onClick={() => {
                  setDraft(lastPromptRef.current);
                  setChatError(null);
                }}
                className="tap-target rounded-card border border-current px-3 text-xs"
              >
                重试
              </button>
            }
          >
            {chatError}
          </Banner>
        )}
        {hint && <Banner variant="info">{hint}</Banner>}
      </div>

      {/* 消息流 */}
      <div
        ref={scrollRef}
        onScroll={onScroll}
        className="ah-scroll min-h-0 min-w-0 flex-1 overflow-y-auto px-3 py-4"
        aria-live="polite"
      >
        {loadingHistory ? (
          <Spinner label="加载历史消息…" />
        ) : messages.length === 0 ? (
          <EmptyState
            title="还没有消息"
            description="在下方输入内容并发送，回复会以流式增量逐字出现。"
          />
        ) : (
          <div className="flex min-w-0 flex-col">
            {messages.map((message) => {
              const isUser = message.role === ROLE.USER;
              const label = statusLabel(message.status);
              return (
                <div
                  key={message.key}
                  className={clsx('mb-3 flex min-w-0', isUser ? 'justify-end' : 'justify-start')}
                >
                  <div
                    className={clsx(
                      'min-w-0 max-w-[85%] whitespace-pre-wrap break-words rounded-card border px-3 py-2 text-sm',
                      isUser
                        ? 'border-transparent bg-brand text-white'
                        : 'border-line bg-surface text-ink',
                      message.status === MESSAGE_STATUS.STREAMING && 'ah-cursor',
                    )}
                  >
                    {message.content ||
                      (message.status === MESSAGE_STATUS.STREAMING ? (
                        <span className="text-muted">正在生成…</span>
                      ) : null)}
                    {label && message.status !== MESSAGE_STATUS.STREAMING && (
                      <p className="mt-1 text-2xs opacity-70">{label}</p>
                    )}
                    {message.status === MESSAGE_STATUS.ERROR && message.error && (
                      <ErrorDetail text={message.error} />
                    )}
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </div>

      {/* 输入区：sticky 底部 + safe-area 避让；输入框 ≥16px 防 iOS 缩放 */}
      <div className="sticky bottom-0 shrink-0 border-t border-line bg-surface px-3 pt-2 ah-safe-bottom">
        <div className="flex min-w-0 items-end gap-2">
          <textarea
            rows={1}
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            onKeyDown={onTextareaKeyDown}
            placeholder={connected ? '输入消息…' : '连接断开，暂不可发送'}
            aria-label="输入消息"
            className="ah-scroll max-h-32 min-h-[44px] min-w-0 flex-1 resize-none rounded-card border border-line bg-surface px-3 py-2 text-base text-ink placeholder:text-muted"
          />
          {generating ? (
            <button
              type="button"
              onClick={handleStop}
              className="tap-target flex shrink-0 items-center justify-center gap-1 rounded-card border border-line bg-surface px-3 text-xs font-medium text-danger"
            >
              <Square size={14} aria-hidden="true" />
              停止生成
            </button>
          ) : (
            <button
              type="button"
              onClick={handleSend}
              disabled={!canSend}
              className="tap-target flex shrink-0 items-center justify-center gap-1 rounded-card bg-brand px-3 text-xs font-medium text-white disabled:opacity-50"
            >
              <Send size={14} aria-hidden="true" />
              发送
            </button>
          )}
        </div>
        <p className="mt-1 text-2xs text-muted">
          <span className="hidden md:inline">Enter 发送 / Shift+Enter 换行</span>
          <span className="md:hidden">点击「发送」按钮发送</span>
        </p>
      </div>
    </div>
  );
}
