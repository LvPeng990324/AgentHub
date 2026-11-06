import { useCallback, useEffect, useState } from 'react';

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
import { ConfirmDialog } from '../components/ConfirmDialog';
import { EmptyState } from '../components/EmptyState';
import { RemarkEditor } from '../components/RemarkEditor';
import { SearchFilterBar, type FilterGroup } from '../components/SearchFilterBar';
import { Spinner } from '../components/Spinner';
import { StatusDot } from '../components/StatusDot';
import { TypeBadge } from '../components/TypeBadge';
import { useClientWS } from '../ws/ClientWS';

const TYPE_OPTIONS = Object.values(AGENT_TYPE).map((value) => ({
  value,
  label: AGENT_TYPE_LABELS[value] ?? value,
}));

/** P-3 Agent 发现与管理页：全量已发现（含未注册）、批量注册、取消注册二次确认、备注、筛选。 */
export function DiscoverPage() {
  const { subscribe } = useClientWS();
  const [agents, setAgents] = useState<AgentDTO[]>([]);
  const [devices, setDevices] = useState<DeviceDTO[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [acting, setActing] = useState(false);

  const [q, setQ] = useState('');
  const [typeFilter, setTypeFilter] = useState('');
  const [onlineFilter, setOnlineFilter] = useState('');
  const [deviceFilter, setDeviceFilter] = useState('');
  const [registeredFilter, setRegisteredFilter] = useState('');
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [confirmAgent, setConfirmAgent] = useState<AgentDTO | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.listDiscovered({
        q: q || undefined,
        type: typeFilter || undefined,
        online: onlineFilter === '' ? undefined : onlineFilter === 'true',
        device_id: deviceFilter || undefined,
        registered: registeredFilter === '' ? undefined : registeredFilter === 'true',
        limit: MAX_LIST_LIMIT,
      });
      setAgents(data.items);
      setTotal(data.total);
    } catch (caught) {
      setError(toErrorMessage(caught));
    } finally {
      setLoading(false);
    }
  }, [q, typeFilter, onlineFilter, deviceFilter, registeredFilter]);

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

  // 筛选状态变化时清空勾选，避免对已不在结果集的 id 执行批量操作。
  useEffect(() => {
    setSelected(new Set());
  }, [q, typeFilter, onlineFilter, deviceFilter, registeredFilter]);

  const toggleSelect = (agentId: string) => {
    setSelected((previous) => {
      const next = new Set(previous);
      if (next.has(agentId)) next.delete(agentId);
      else next.add(agentId);
      return next;
    });
  };

  const unregistered = agents.filter((agent) => !agent.registered);
  const allUnregisteredSelected =
    unregistered.length > 0 && unregistered.every((agent) => selected.has(agent.id));

  const toggleSelectAll = () => {
    setSelected(allUnregisteredSelected ? new Set() : new Set(unregistered.map((a) => a.id)));
  };

  const registerOne = async (agent: AgentDTO) => {
    setActing(true);
    setError(null);
    try {
      await api.registerAgent(agent.id);
      setNotice(`已注册「${agent.name}」，现在可在 Agent 列表中找到它`);
      await load();
    } catch (caught) {
      setError(toErrorMessage(caught));
    } finally {
      setActing(false);
    }
  };

  const batchRegister = async () => {
    const ids = Array.from(selected).filter(
      (id) => agents.find((agent) => agent.id === id)?.registered === false,
    );
    if (ids.length === 0) {
      setNotice('请先勾选尚未注册的 agent');
      return;
    }
    setActing(true);
    setError(null);
    try {
      const result = await api.batchRegister(ids);
      setNotice(
        `已注册 ${result.registered.length} 个 agent` +
          (result.skipped.length > 0 ? `，跳过 ${result.skipped.length} 个不存在的 id` : ''),
      );
      setSelected(new Set());
      await load();
    } catch (caught) {
      setError(toErrorMessage(caught));
    } finally {
      setActing(false);
    }
  };

  const doUnregister = async () => {
    if (!confirmAgent) return;
    setActing(true);
    setError(null);
    try {
      const result = await api.unregisterAgent(confirmAgent.id);
      setNotice(
        `已取消注册「${confirmAgent.name}」，` +
          (result.messages_cleared ? '该 Agent 的全部会话数据已清空' : '会话数据未清空'),
      );
      setConfirmAgent(null);
      await load();
    } catch (caught) {
      setError(toErrorMessage(caught));
    } finally {
      setActing(false);
    }
  };

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
      key: 'registered',
      label: '注册',
      value: registeredFilter,
      options: [
        { value: 'true', label: '已注册' },
        { value: 'false', label: '未注册' },
      ],
      onChange: setRegisteredFilter,
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
    <div className="flex min-w-0 flex-col gap-3">
      <div className="flex min-w-0 flex-wrap items-center justify-between gap-2">
        <div className="min-w-0">
          <h1 className="text-base font-semibold text-ink">Agent 发现与管理</h1>
          <p className="text-2xs text-muted">共发现 {total} 个 agent（含未注册）</p>
        </div>
        <button
          type="button"
          onClick={() => void batchRegister()}
          disabled={acting || selected.size === 0}
          className="tap-target shrink-0 rounded-card bg-brand px-4 text-xs font-medium text-white disabled:opacity-50"
        >
          批量注册{selected.size > 0 ? ` (${selected.size})` : ''}
        </button>
      </div>

      <SearchFilterBar
        q={q}
        onQueryChange={setQ}
        groups={filterGroups}
        placeholder="搜索名称 / 备注 / 设备"
      />

      <label className="flex min-h-[44px] items-center gap-2 text-xs text-muted">
        <input
          type="checkbox"
          checked={allUnregisteredSelected}
          onChange={toggleSelectAll}
          className="h-5 w-5 shrink-0"
        />
        全选未注册 ({unregistered.length})
      </label>

      {notice && (
        <Banner variant="success" title="操作完成">
          {notice}
        </Banner>
      )}
      {error && <Banner variant="error">{error}</Banner>}

      {loading ? (
        <Spinner />
      ) : agents.length === 0 ? (
        <EmptyState
          title="没有匹配的 agent"
          description="调整搜索或筛选条件；新设备接入后会自动出现在发现列表。"
        />
      ) : (
        <div className="flex min-w-0 flex-col gap-2">
          {agents.map((agent) => (
            <article key={agent.id} className="ah-card min-w-0 p-3">
              <div className="flex min-w-0 items-start gap-2">
                <label
                  className="flex min-h-[44px] min-w-[44px] shrink-0 cursor-pointer items-start justify-center pt-2"
                  title={agent.registered ? '已注册的 agent 无需勾选' : '选择以批量注册'}
                >
                  <input
                    type="checkbox"
                    checked={selected.has(agent.id)}
                    onChange={() => toggleSelect(agent.id)}
                    disabled={agent.registered}
                    className="h-5 w-5 disabled:cursor-not-allowed"
                    aria-label={`选择 ${agent.name}`}
                  />
                </label>
                <div className="min-w-0 flex-1">
                  <div className="flex min-w-0 flex-wrap items-center gap-2">
                    <h2 className="min-w-0 truncate text-sm font-medium text-ink">{agent.name}</h2>
                    <TypeBadge type={agent.type} displayName={agent.display_name} />
                    <StatusDot online={agent.online} />
                    <span className="text-2xs text-muted">
                      {agent.registered ? '已注册' : '未注册'}
                    </span>
                  </div>
                  <p className="mt-1 truncate text-2xs text-muted" title={agent.device_name}>
                    设备：{agent.device_name || agent.device_id}
                  </p>
                  <div className="mt-2 border-t border-line pt-2">
                    <RemarkEditor
                      value={agent.remark}
                      label="Agent 备注"
                      onSave={async (remark: string) => {
                        await api.updateAgentRemark(agent.id, remark);
                        await load();
                      }}
                    />
                  </div>
                  <div className="mt-2 flex flex-wrap items-center gap-2">
                    {agent.registered ? (
                      <button
                        type="button"
                        onClick={() => setConfirmAgent(agent)}
                        disabled={acting}
                        className="tap-target rounded-card border border-red-200 bg-red-50 px-3 text-xs font-medium text-danger disabled:opacity-50"
                      >
                        取消注册
                      </button>
                    ) : (
                      <button
                        type="button"
                        onClick={() => void registerOne(agent)}
                        disabled={acting}
                        className="tap-target rounded-card bg-brand px-3 text-xs font-medium text-white disabled:opacity-50"
                      >
                        注 册
                      </button>
                    )}
                    <span className="truncate text-2xs text-muted" title={agent.version}>
                      v{agent.version || 'unknown'}
                    </span>
                  </div>
                </div>
              </div>
            </article>
          ))}
        </div>
      )}

      <ConfirmDialog
        open={confirmAgent !== null}
        title={`取消注册「${confirmAgent?.name ?? ''}」？`}
        description={
          <>
            <p className="font-medium text-danger">
              此操作将清空该 Agent 的全部会话数据，且不可恢复。
            </p>
            <p className="mt-1">设备连接与其它 agent 不受影响，之后仍可重新注册。</p>
          </>
        }
        confirmText="确认清空"
        danger
        busy={acting}
        onCancel={() => setConfirmAgent(null)}
        onConfirm={() => void doUnregister()}
      />
    </div>
  );
}
