import clsx from 'clsx';
import { Bot, Compass, LogOut, Menu, Monitor, X } from 'lucide-react';
import { useCallback, useEffect, useState } from 'react';
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom';

import { api } from '../api/client';
import { useAuth } from '../store/AuthContext';
import { useClientWS } from '../ws/ClientWS';
import { Banner } from './Banner';

const NAV_ITEMS = [
  { to: '/agents', label: 'Agent 列表', icon: Bot },
  { to: '/devices', label: '设备', icon: Monitor },
  { to: '/discover', label: '发现与管理', icon: Compass },
] as const;

interface NavContentProps {
  stats: { onlineDevices: number; registeredAgents: number };
  onNavigate: () => void;
  onLogout: () => void;
}

/** 桌面侧栏与移动抽屉共用同一份导航内容。 */
function NavContent({ stats, onNavigate, onLogout }: NavContentProps) {
  return (
    <>
      <div className="px-4 py-4">
        <p className="text-sm font-semibold text-ink">AgentHub</p>
        <p className="text-2xs text-muted">集中管理你的 AI Agent</p>
      </div>
      <nav className="flex flex-col gap-1 px-2" aria-label="主导航">
        {NAV_ITEMS.map((item) => {
          const Icon = item.icon;
          return (
            <NavLink
              key={item.to}
              to={item.to}
              onClick={onNavigate}
              className={({ isActive }) =>
                clsx(
                  'tap-target flex items-center gap-2 rounded-card px-3 text-sm',
                  isActive
                    ? 'bg-brand-soft font-medium text-brand'
                    : 'text-ink hover:bg-surface-muted',
                )
              }
            >
              <Icon size={18} aria-hidden="true" />
              <span className="truncate">{item.label}</span>
            </NavLink>
          );
        })}
      </nav>
      <div className="mt-auto border-t border-line px-4 py-3 ah-safe-bottom">
        <p className="text-2xs text-muted">
          在线设备 {stats.onlineDevices} · 已注册 {stats.registeredAgents}
        </p>
        <button
          type="button"
          onClick={onLogout}
          className="tap-target mt-2 flex w-full items-center gap-2 rounded-card px-1 text-xs text-muted hover:text-danger"
        >
          <LogOut size={16} aria-hidden="true" />
          退出登录
        </button>
      </div>
    </>
  );
}

/**
 * 应用壳（§6.1 / §10.6）：
 * ≥md 为「Rail(56px) + 侧栏(260px) + 内容」；<md 为顶部栏 + 左侧抽屉 + 底部导航。
 * 对话页在 <md 下全屏（无抽屉与底部导航），由 ChatPage 自己提供吸顶会话头。
 * 路由切换自动关闭抽屉；WS 断线时顶部持久横幅。
 */
export function AppShell() {
  const { logout } = useAuth();
  const { connectionState, statusEpoch, reconnectNow } = useClientWS();
  const location = useLocation();
  const navigate = useNavigate();
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [stats, setStats] = useState({ onlineDevices: 0, registeredAgents: 0 });

  const isChat = location.pathname.startsWith('/chat/');

  useEffect(() => {
    setDrawerOpen(false);
  }, [location.pathname]);

  const refreshStats = useCallback(async () => {
    try {
      const [devices, agents] = await Promise.all([
        api.listDevices({ limit: 500 }),
        api.listAgents({ limit: 500 }),
      ]);
      setStats({
        onlineDevices: devices.items.filter((device) => device.online).length,
        registeredAgents: agents.total,
      });
    } catch {
      // 侧栏统计失败不阻塞主内容。
    }
  }, []);

  useEffect(() => {
    void refreshStats();
  }, [refreshStats, statusEpoch, connectionState]);

  const handleLogout = useCallback(() => {
    logout();
    navigate('/login', { replace: true });
  }, [logout, navigate]);

  const closeDrawer = useCallback(() => setDrawerOpen(false), []);
  const offline = connectionState !== 'connected';

  return (
    <div className="flex h-full min-h-0 w-full overflow-x-hidden bg-surface-muted">
      {/* Rail：仅桌面 */}
      <aside className="hidden w-14 shrink-0 flex-col items-center border-r border-line bg-surface py-3 md:flex">
        <div className="flex h-9 w-9 items-center justify-center rounded-card bg-brand text-white">
          <Bot size={20} aria-hidden="true" />
        </div>
        <span className="mt-2 text-[10px] text-muted">AH</span>
      </aside>

      {/* 侧栏：仅桌面 */}
      <aside className="hidden w-[260px] shrink-0 flex-col border-r border-line bg-surface md:flex">
        <NavContent stats={stats} onNavigate={closeDrawer} onLogout={handleLogout} />
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        {/* 移动顶部栏：对话页隐藏 */}
        {!isChat && (
          <header className="flex h-14 shrink-0 items-center gap-2 border-b border-line bg-surface px-2 md:hidden">
            <button
              type="button"
              onClick={() => setDrawerOpen(true)}
              className="tap-target flex items-center justify-center rounded-card text-ink"
              aria-label="打开导航菜单"
            >
              <Menu size={22} aria-hidden="true" />
            </button>
            <span className="min-w-0 flex-1 truncate text-sm font-semibold">AgentHub</span>
            <span
              className={clsx(
                'mr-1 inline-block h-2 w-2 rounded-full',
                offline ? 'bg-slate-300' : 'bg-success',
              )}
              aria-label={offline ? '连接断开' : '连接正常'}
            />
          </header>
        )}

        {!isChat && offline && (
          <div className="px-3 pt-2 md:px-5">
            <Banner
              variant={connectionState === 'unauthorized' ? 'error' : 'offline'}
              title={connectionState === 'unauthorized' ? '登录态已失效' : '实时连接已断开'}
              action={
                <button
                  type="button"
                  onClick={reconnectNow}
                  className="tap-target rounded-card border border-current px-3 text-xs font-medium"
                >
                  立即重连
                </button>
              }
            >
              {connectionState === 'unauthorized'
                ? '请重新登录后继续使用。'
                : '正在按退避策略自动重连，恢复后会自动补齐历史消息。'}
            </Banner>
          </div>
        )}

        <main
          className={clsx(
            'ah-scroll min-w-0 flex-1',
            isChat ? 'overflow-hidden' : 'overflow-y-auto p-3 md:p-5',
          )}
        >
          <Outlet />
        </main>

        {/* 移动底部导航：对话页隐藏 */}
        {!isChat && (
          <nav
            className="flex shrink-0 items-stretch border-t border-line bg-surface md:hidden ah-safe-bottom"
            aria-label="底部导航"
          >
            {NAV_ITEMS.map((item) => {
              const Icon = item.icon;
              return (
                <NavLink
                  key={item.to}
                  to={item.to}
                  className={({ isActive }) =>
                    clsx(
                      'tap-target flex min-w-0 flex-1 flex-col items-center justify-center gap-0.5 text-[11px]',
                      isActive ? 'text-brand' : 'text-muted',
                    )
                  }
                >
                  <Icon size={18} aria-hidden="true" />
                  <span className="truncate">{item.label}</span>
                </NavLink>
              );
            })}
          </nav>
        )}
      </div>

      {/* 移动抽屉：路由切换自动关闭；对话页不提供 */}
      {!isChat && (
        <div
          className={clsx('fixed inset-0 z-40 md:hidden', drawerOpen ? 'block' : 'hidden')}
          aria-hidden={!drawerOpen}
        >
          <button
            type="button"
            className="absolute inset-0 h-full w-full bg-black/30"
            onClick={closeDrawer}
            aria-label="关闭导航菜单"
          />
          <aside className="absolute inset-y-0 left-0 flex w-[min(82vw,300px)] flex-col border-r border-line bg-surface shadow-xl ah-safe-left animate-fade-in">
            <div className="flex items-center justify-end px-2 pt-2">
              <button
                type="button"
                onClick={closeDrawer}
                className="tap-target flex items-center justify-center rounded-card text-muted"
                aria-label="关闭导航菜单"
              >
                <X size={20} aria-hidden="true" />
              </button>
            </div>
            <NavContent stats={stats} onNavigate={closeDrawer} onLogout={handleLogout} />
          </aside>
        </div>
      )}
    </div>
  );
}
