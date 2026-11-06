import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react';

import { api, clearToken, getToken, setToken, setUnauthorizedHandler } from '../api/client';

/** 登录态是全应用唯一全局状态（§10.3），token 为不透明串，存 localStorage。 */
interface AuthContextValue {
  token: string | null;
  isAuthenticated: boolean;
  login: (password: string) => Promise<void>;
  logout: () => void;
  /** 鉴权失败 / token 过期 / WS 握手失败时清 token，由 ProtectedRoute 触发跳登录。 */
  clearSession: () => void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [token, setTokenState] = useState<string | null>(() => getToken());

  const clearSession = useCallback(() => {
    clearToken();
    setTokenState(null);
  }, []);

  // client.ts 收到 401 时回调清态，ProtectedRoute 随即重定向到 /login。
  useEffect(() => {
    setUnauthorizedHandler(clearSession);
    return () => setUnauthorizedHandler(null);
  }, [clearSession]);

  const login = useCallback(async (password: string) => {
    const data = await api.login(password);
    setToken(data.token);
    setTokenState(data.token);
  }, []);

  const logout = useCallback(() => {
    // 先发出登出请求（此刻 token 仍在），再同步清本地态，保证即时跳登录。
    void api.logout().catch(() => undefined);
    clearSession();
  }, [clearSession]);

  const value = useMemo<AuthContextValue>(
    () => ({ token, isAuthenticated: token !== null, login, logout, clearSession }),
    [token, login, logout, clearSession],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const context = useContext(AuthContext);
  if (!context) {
    throw new Error('useAuth 必须在 AuthProvider 内使用');
  }
  return context;
}
