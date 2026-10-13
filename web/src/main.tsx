import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import './theme.css';
import { applyTheme } from './theme';
import { App, SkipLink, probeAccount } from './App';
import { ToastHost } from './components/ui';
import { connectEvents } from './api/sse';
import { taskStoreHooks } from './stores/tasks';

// D7：启动即按系统配色挂主题；设置页锁定后由 applyTheme(lock) 重挂（Task 9）。
applyTheme('');

// 启动时序（简报钉死）：先探测 /api/account 写 session store，再连 SSE。
// 会话过期跳转由守卫响应 loggedIn=false 完成（守卫是唯一导航权威）——
// connectEvents 不注入 navigate，避免与守卫双通道跳转导致 location 二次变更。
// Task 7：注入任务 store 三方法适配器（TaskStoreHooks 本体在 stores/tasks.ts），
// hello 对账 / progress / final 帧自此驱动任务视图。
void probeAccount().then(() => {
  connectEvents({ tasks: taskStoreHooks });
});

const queryClient = new QueryClient();

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <SkipLink />
        <App />
        <ToastHost />
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
