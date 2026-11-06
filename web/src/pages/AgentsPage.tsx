import { ArrowRight } from 'lucide-react';
import { useCallback, useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';

import { api, toErrorMessage } from '../api/client';
import {
  AGENT_TYPE,
  AGENT_TYPE_LABELS,
  FRAME,
  MAX_LIST_LIMIT,
  type AgentDTO,
  type DeviceDTO,
} from '../api/types';
import { Banner } from '../components/Banner';
import { EmptyState } from '../components/EmptyState';
import { SearchFilterBar, type FilterGroup } from '../components/SearchFilterBar';
import { Spinner } from '../components/Spinner';
import { StatusDot } from '../components/StatusDot';
import { TypeBadge } from '../components/TypeBadge';
import { useClientWS } from '../ws/ClientWS';

const TYPE_OPTIONS = Object.values(AGENT_TYPE).map((value) => ({
  value,
  label: AGENT_TYPE_LABELS[value] ?? value,
}));

/** P-4 Agent 列表页（默认首页）：仅已注册；卡片四要素齐全；离线禁用进入对话。 */
export function AgentsPage() {
  const navigate = useNavigate();
  const { subscribe } = useClientWS();
  const [agents, setAgents] = useState<AgentDTO[]>([]);
  const [devices, setDevices] = useState<DeviceDTO[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [q, setQ] = useState('');
  const [typeFilter, setTypeFilter] = useState('');
  const [onlineFilter, setOnlineFilter] = useState('');
  const [deviceFilter, setDeviceFilter] = useState('');

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      // E09 服务端已限定 registered=1；其余筛选与 ParseListQuery 一致。
      const data = await api.listAgents({
        q: q || undefined,
        type: typeFilter || undefined,
        online: onlineFilter === '' ? undefined : onlineFilter === 'true',
        device_id: deviceFilter || undefined,
        limit: MAX_LIST_LIMIT,
      });
      setAgents(data.items);
      setTotal(data.total);
    } catch (caught) {
      setError(toErrorMessage(caught));
    } finally {
      setLoading(false);
    }
  }, [q, typeFilter, onlineFilter, deviceFilter]);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    let alive = true;
    void api
      .listDevices({ limit: MAX_LIST_LIMIT })
      .then((data) => {
        if (alive) setDevices(data.items);
      })
      .catch(() => undefined);
    return () => {
      alive = false;
    };
  }, []);

  useEffect(() => {
    const off = subscribe(FRAME.AGENT_STATUS, () => void load());
    return () => off();
  }, [subscribe, load]);

  const filterGroups: FilterGroup[] = [
    { key: 'type', label: '类型', value: typeFilter, options: TYPE_OPTIONS, onChange: setTypeFilter },
    {
      key: 'online',
      label: '在线',
      value: onlineFilter,
      options: [
        { value: 'true', label: '在线' },
        { value: 'false', label: '离线' },
      ],
      onChange: setOnlineFilter,
    },
    {
      key: 'device_id',
      label: '设备',
      value: deviceFilter,
      options: devices.map((device) => ({ value: device.id, label: device.name })),
      onChange: setDeviceFilter,
    },
  ];

  return (
    <div className="flex min-w-0 flex-col gap-3 pb-2">
      <div className="flex min-w-0 flex-wrap items-center justify-between gap-2">
        <div className="min-w-0">
          <h1 className="text-base font-semibold text-ink">Agent 列表</h1>
          <p className="text-2xs text-muted">共 {total} 个已注册 agent</p>
        </div>
        <button
          type="button"
          onClick={() => navigate('/discover')}
          className="tap-target shrink-0 rounded-card border border-line bg-surface px-3 text-xs text-brand"
        >
          去发现与管理
        </button>
      </div>

      <SearchFilterBar
        q={q}
        onQueryChange={setQ}
        groups={filterGroups}
        placeholder="搜索名称 / 备注 / 设备"
      />

      {error && <Banner variant="error">{error}</Banner>}

      {loading ? (
        <Spinner />
      ) : agents.length === 0 ? (
        <EmptyState
          title="还没有已注册的 agent"
          description="前往「Agent 发现与管理」勾选需要的 agent，注册后即可在这里点击进入对话。"
          action={
            <button
              type="button"
              onClick={() => navigate('/discover')}
              className="tap-target rounded-card bg-brand px-4 text-xs font-medium text-white"
            >
              去注册 Agent
            </button>
          }
        />
      ) : (
        <div className="grid min-w-0 grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
          {agents.map((agent) => (
            <article key={agent.id} className="ah-card flex min-w-0 flex-col gap-2 p-3">
              <div className="flex min-w-0 items-center gap-2">
                <StatusDot online={agent.online} />
                <h2 className="min-w-0 flex-1 truncate text-sm font-semibold text-ink">
                  {agent.name}
                </h2>
              </div>
              <div className="flex min-w-0 items-center gap-2">
                <TypeBadge type={agent.type} displayName={agent.display_name} />
              </div>
              <p className="min-w-0 truncate text-xs text-muted" title={agent.device_name}>
                所属设备：{agent.device_name || agent.device_id}
              </p>
              <p className="min-w-0 truncate text-2xs text-muted" title={agent.remark}>
                备注：{agent.remark || '—'}
              </p>
              <button
                type="button"
                disabled={!agent.online}
                onClick={() => navigate(`/chat/${encodeURIComponent(agent.id)}`)}
                title={agent.online ? '进入对话' : '设备离线，暂时无法发送消息'}
                className="tap-target mt-auto flex w-full items-center justify-center gap-1 rounded-card border border-line bg-surface text-xs font-medium text-brand disabled:cursor-not-allowed disabled:border-transparent disabled:bg-surface-muted disabled:text-muted"
              >
                {agent.online ? (
                  <>
                    进入对话 <ArrowRight size={14} aria-hidden="true" />
                  </>
                ) : (
                  '离线，暂不可用'
                )}
              </button>
            </article>
          ))}
        </div>
      )}
    </div>
  );
}
