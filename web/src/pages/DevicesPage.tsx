import clsx from 'clsx';
import { ChevronDown } from 'lucide-react';
import { useCallback, useEffect, useState } from 'react';

import { api, formatRelativeTime, toErrorMessage } from '../api/client';
import { FRAME, MAX_LIST_LIMIT, type DeviceDTO } from '../api/types';
import { Banner } from '../components/Banner';
import { EmptyState } from '../components/EmptyState';
import { RemarkEditor } from '../components/RemarkEditor';
import { SearchFilterBar } from '../components/SearchFilterBar';
import { Spinner } from '../components/Spinner';
import { StatusDot } from '../components/StatusDot';
import { TypeBadge } from '../components/TypeBadge';
import { useClientWS } from '../ws/ClientWS';

interface DeviceCardProps {
  device: DeviceDTO;
  expanded: boolean;
  onToggle: () => void;
  onSaveRemark: (remark: string) => Promise<void>;
}

/** 单台设备卡片；展开项含 Q14 只读 version / exec_path。 */
function DeviceCard({ device, expanded, onToggle, onSaveRemark }: DeviceCardProps) {
  return (
    <article
      className={clsx(
        'ah-card min-w-0 p-3',
        !device.online && 'opacity-75',
      )}
    >
      <div className="flex min-w-0 flex-wrap items-center gap-2">
        <StatusDot online={device.online} />
        <h2 className="min-w-0 flex-1 truncate text-sm font-medium text-ink">{device.name}</h2>
        <span className="shrink-0 text-2xs text-muted">
          {device.os} · {device.arch}
        </span>
      </div>

      <div className="mt-1 flex min-w-0 flex-wrap gap-x-3 gap-y-0.5 text-2xs text-muted">
        <span>最后心跳 {formatRelativeTime(device.last_seen_at)}</span>
        <span>适配器 v{device.adapter_version || '—'}</span>
        <span>协议 v{device.protocol_version}</span>
      </div>

      <div className="mt-2 border-t border-line pt-2">
        <RemarkEditor
          value={device.remark}
          onSave={onSaveRemark}
          label="设备备注"
          placeholder="添加设备备注…"
        />
      </div>

      <button
        type="button"
        onClick={onToggle}
        aria-expanded={expanded}
        className="tap-target mt-1 flex w-full items-center justify-between gap-2 text-xs text-brand"
      >
        <span className="min-w-0 truncate">
          {device.agents.length} 个 agent · {expanded ? '收起' : '点击查看版本与路径'}
        </span>
        <ChevronDown
          size={16}
          className={clsx('shrink-0 transition-transform', expanded && 'rotate-180')}
          aria-hidden="true"
        />
      </button>

      {expanded && (
        <ul className="mt-2 flex min-w-0 flex-col gap-2 border-t border-line pt-2">
          {device.agents.length === 0 && (
            <li className="text-xs text-muted">该设备暂未发现 agent</li>
          )}
          {device.agents.map((agent) => (
            <li key={agent.id} className="flex min-w-0 flex-col gap-1 rounded-card bg-surface-muted p-2">
              <div className="flex min-w-0 flex-wrap items-center gap-2">
                <TypeBadge type={agent.type} displayName={agent.display_name} />
                <StatusDot online={agent.online} />
                <span className="text-2xs text-muted">
                  {agent.registered ? '已注册' : '未注册'}
                </span>
              </div>
              <p className="truncate text-2xs text-muted" title={agent.version}>
                版本：{agent.version || 'unknown'}
              </p>
              <p className="truncate text-2xs text-muted" title={agent.exec_path}>
                路径：{agent.exec_path || '—'}
              </p>
            </li>
          ))}
        </ul>
      )}
    </article>
  );
}

/** P-2 设备页：设备清单 + 展开 agent 摘要 + 备注 + 搜索（R-04 / R-05 / R-17）。 */
export function DevicesPage() {
  const { subscribe } = useClientWS();
  const [devices, setDevices] = useState<DeviceDTO[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [q, setQ] = useState('');
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.listDevices({ q: q || undefined, limit: MAX_LIST_LIMIT });
      setDevices(data.items);
      setTotal(data.total);
    } catch (caught) {
      setError(toErrorMessage(caught));
    } finally {
      setLoading(false);
    }
  }, [q]);

  useEffect(() => {
    void load();
  }, [load]);

  // F15 device.status / F11 agent.status 实时刷新，无需手动刷新（R-12）。
  useEffect(() => {
    const offDevice = subscribe(FRAME.DEVICE_STATUS, () => void load());
    const offAgent = subscribe(FRAME.AGENT_STATUS, () => void load());
    return () => {
      offDevice();
      offAgent();
    };
  }, [subscribe, load]);

  const offlineCount = devices.filter((device) => !device.online).length;

  return (
    <div className="flex min-w-0 flex-col gap-3">
      <div className="flex min-w-0 flex-wrap items-center justify-between gap-2">
        <h1 className="text-base font-semibold text-ink">设备 ({total})</h1>
        <p className="text-2xs text-muted">已连接过的设备及其发现的 agent</p>
      </div>

      <SearchFilterBar q={q} onQueryChange={setQ} placeholder="搜索设备名称 / 备注" />

      {offlineCount > 0 && (
        <Banner variant="warning" title={`${offlineCount} 台设备离线`}>
          离线设备下的 agent 暂不可发送消息，恢复连接后自动变回在线。
        </Banner>
      )}
      {error && <Banner variant="error">{error}</Banner>}

      {loading ? (
        <Spinner />
      ) : devices.length === 0 ? (
        <EmptyState
          title="暂无设备"
          description="请在目标设备上启动连接适配器，发现结果会在一个周期内出现在这里。"
        />
      ) : (
        <div className="flex min-w-0 flex-col gap-2">
          {devices.map((device) => (
            <DeviceCard
              key={device.id}
              device={device}
              expanded={Boolean(expanded[device.id])}
              onToggle={() =>
                setExpanded((previous) => ({ ...previous, [device.id]: !previous[device.id] }))
              }
              onSaveRemark={async (remark: string) => {
                await api.updateDeviceRemark(device.id, remark);
                await load();
              }}
            />
          ))}
        </div>
      )}
    </div>
  );
}
