import type { ReactNode } from 'react';
import { Navigate, Route, Routes, useLocation } from 'react-router-dom';

import { AppShell } from './components/AppShell';
import { AgentsPage } from './pages/AgentsPage';
import { ChatPage } from './pages/ChatPage';
import { DevicesPage } from './pages/DevicesPage';
import { DiscoverPage } from './pages/DiscoverPage';
import { LoginPage } from './pages/LoginPage';
import { useAuth } from './store/AuthContext';

/** 未登录访问 P-2~P-5 一律重定向到登录页（R-01）。 */
function ProtectedRoute({ children }: { children: ReactNode }) {
  const { isAuthenticated } = useAuth();
  const location = useLocation();
  if (!isAuthenticated) {
    return <Navigate to="/login" replace state={{ from: location.pathname }} />;
  }
  return <>{children}</>;
}

/** 路由表（§10.1）：/agents 为默认首页，P-1 不套 AppShell。 */
export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/" element={<Navigate to="/agents" replace />} />
      <Route
        element={
          <ProtectedRoute>
            <AppShell />
          </ProtectedRoute>
        }
      >
        <Route path="/agents" element={<AgentsPage />} />
        <Route path="/devices" element={<DevicesPage />} />
        <Route path="/discover" element={<DiscoverPage />} />
        <Route path="/chat/:agentId" element={<ChatPage />} />
      </Route>
      <Route path="*" element={<Navigate to="/agents" replace />} />
    </Routes>
  );
}
