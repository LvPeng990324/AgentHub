import { Bot } from 'lucide-react';
import { useEffect, useState, type FormEvent } from 'react';
import { Navigate, useNavigate } from 'react-router-dom';

import { api, toErrorMessage } from '../api/client';
import type { MetaData } from '../api/types';
import { useAuth } from '../store/AuthContext';

/** P-1 登录页：单一密码 + 公开的 /meta 展示平台名与域名（R-01）。 */
export function LoginPage() {
  const { isAuthenticated, login } = useAuth();
  const navigate = useNavigate();
  const [password, setPassword] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [meta, setMeta] = useState<MetaData | null>(null);

  useEffect(() => {
    let alive = true;
    void api
      .getMeta()
      .then((data) => {
        if (alive) setMeta(data);
      })
      .catch(() => undefined);
    return () => {
      alive = false;
    };
  }, []);

  if (isAuthenticated) {
    return <Navigate to="/agents" replace />;
  }

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!password || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      await login(password);
      navigate('/agents', { replace: true });
    } catch (caught) {
      setError(toErrorMessage(caught));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="flex min-h-full w-full items-center justify-center overflow-x-hidden p-4">
      <main className="ah-card w-full max-w-[400px] min-w-0 p-6">
        <div className="flex flex-col items-center gap-2 text-center">
          <span className="flex h-11 w-11 items-center justify-center rounded-card bg-brand text-white">
            <Bot size={24} aria-hidden="true" />
          </span>
          <h1 className="text-lg font-semibold text-ink">{meta?.name ?? 'AgentHub'}</h1>
          <p className="text-xs text-muted">集中管理你的 AI Agent</p>
          {meta?.domain && (
            <p className="break-all text-2xs text-muted">
              {meta.domain} · v{meta.version}
            </p>
          )}
        </div>

        <form onSubmit={(event) => void handleSubmit(event)} className="mt-6 flex flex-col gap-3">
          <label htmlFor="login-password" className="text-xs font-medium text-ink">
            登录密码
          </label>
          <input
            id="login-password"
            type="password"
            value={password}
            autoComplete="current-password"
            onChange={(event) => setPassword(event.target.value)}
            placeholder="请输入登录密码"
            className="min-h-[48px] w-full rounded-card border border-line bg-surface px-3 text-base text-ink placeholder:text-muted"
          />
          {error && (
            <p role="alert" className="text-xs text-danger">
              {error}
            </p>
          )}
          <button
            type="submit"
            disabled={submitting || password.length === 0}
            className="tap-target min-h-[48px] w-full rounded-card bg-brand text-sm font-medium text-white disabled:opacity-50"
          >
            {submitting ? '登录中…' : '登 录'}
          </button>
        </form>
        <p className="mt-4 text-center text-2xs text-muted">
          密码由平台配置文件设置，仅用于网页登录
        </p>
      </main>
    </div>
  );
}
