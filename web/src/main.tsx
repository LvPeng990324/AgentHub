import React from 'react';
import ReactDOM from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';

import App from './App';
import { AuthProvider } from './store/AuthContext';
import { ClientWSProvider } from './ws/ClientWS';
import './index.css';

const container = document.getElementById('root');
if (!container) {
  throw new Error('缺少 #root 挂载点，请检查 web/index.html');
}

ReactDOM.createRoot(container).render(
  <React.StrictMode>
    <BrowserRouter>
      <AuthProvider>
        {/* 全应用唯一一条 ws/client 连接，挂在 App 层，切页不断线（§10.3） */}
        <ClientWSProvider>
          <App />
        </ClientWSProvider>
      </AuthProvider>
    </BrowserRouter>
  </React.StrictMode>,
);
