/**
 * 应用壳与路由（Task 4，D2）：路由表 + 登录守卫 + 布局。
 *
 * 守卫三态语义（简报钉死）：loggedIn=null（启动探测中）→ 全屏 Spinner 不跳转；
 * false → <Navigate to="/login" replace>；true → 放行渲染壳。SSE auth 帧
 * expired 由 sse 层 logoutReset 置 false，守卫响应式完成跳转——守卫是唯一
 * 导航权威，connectEvents 不再注入 navigate（避免双通道破坏“只变一次”）。
 *
 * probeAccount：启动探测（main.tsx 编排：先探测后 connectEvents）。探测实现
 * 落在本文件而非 main.tsx 内联——main.tsx 带 createRoot 副作用不可被测试导入；
 * 401/网络错误一律 loggedIn=false（auth.go 未登录即 401 not_logged_in）。
 * 登录态与昵称解耦（T15，校验保守原则）：200 且 logged_in:true 一律视为已
 * 登录，空昵称由 AccountBadge「夸克用户」占位兜底，见 probeAccount 内注释。
 *
 * 页面本体按 Task 接管：/login 已由 pages/Login（Task 5）挂载；/（解析）
 * 已由 pages/Parse（Task 6，控制者预授权接线）挂载；/tasks 已由
 * pages/Tasks（Task 7，控制者预授权接线）挂载；/drive 已由 pages/Drive
 * （Task 8，控制者预授权接线）挂载；/settings 已由 pages/Settings
 * （Task 9，控制者预授权接线）挂载。SkipLink 落在 App.tsx（清单未含独立
 * 文件），main.tsx 与测试共同消费。
 */
import { Navigate, Outlet, Route, Routes } from 'react-router-dom';
import type { ReactNode } from 'react';
import { api } from './api/client';
import type { Account } from './api/types';
import { useSession } from './stores/session';
import { Spinner } from './components/ui';
import { Sidebar } from './layout/Sidebar';
import { Login } from './pages/Login';
import { Parse } from './pages/Parse';
import { Tasks } from './pages/Tasks';
import { Drive } from './pages/Drive';
import { Settings } from './pages/Settings';
import './app.css';

/** 启动探测：GET /api/account 写 session store；失败视为未登录。 */
export async function probeAccount(): Promise<void> {
  try {
    const account = await api.get<Account>('/api/account');
    // 登录态与昵称解耦（T15，校验保守原则，真机实锤）：200 且 logged_in:true
    // 即凭据有效——后端回退链（进程内 cachedAccount → account.json）仍拿不到
    // 昵称时返回 200 {logged_in:true, nickname:"", capacity:{真值}}，这是
    // 「昵称拿不到」而非「登录失效」。撤销上轮「空昵称=未登录」防御（e538df7，
    // 真机复验证实其把有效会话误踢登录页）：空昵称照常 setAccount（容量照常
    // 写入），AccountBadge 以「夸克用户」占位兜底。登录失效只由 401/网络错误
    // 与 SSE expired 帧表达。
    if (account.logged_in) {
      useSession.getState().setAccount({
        nickname: account.nickname,
        avatar: account.avatar,
        capacity: account.capacity,
      });
    } else {
      useSession.getState().setLoggedIn(false);
    }
  } catch {
    // 401 / 网络层错误：语义等价未登录（控制器钉死），不中断启动
    useSession.getState().setLoggedIn(false);
  }
}

/** 登录守卫（三态语义见文件头）。 */
function RequireAuth({ children }: { children: ReactNode }) {
  const loggedIn = useSession((s) => s.loggedIn);

  if (loggedIn === null) {
    return (
      <div className="app-boot" role="status">
        <Spinner />
        <p>正在检查登录状态…</p>
      </div>
    );
  }
  if (!loggedIn) {
    return <Navigate to="/login" replace />;
  }
  return <>{children}</>;
}

/** skill a11y 模式：键盘用户一键越过侧栏导航直达主内容。 */
export function SkipLink() {
  return (
    <a className="skip-link" href="#main-content">
      跳到主内容
    </a>
  );
}

/** D2 壳：左侧固定导航 + 主区；main 为 skip link 唯一落点（Outlet 出页面）。 */
function AppShell() {
  return (
    <div className="app-shell">
      <Sidebar />
      <main id="main-content" className="app-main">
        <Outlet />
      </main>
    </div>
  );
}

/** 路由表（简报钉死五路径）：/login 独立于守卫外，其余经 layout route 全受守卫。 */
export function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route
        element={
          <RequireAuth>
            <AppShell />
          </RequireAuth>
        }
      >
        <Route index element={<Parse />} />
        <Route path="tasks" element={<Tasks />} />
        <Route path="drive" element={<Drive />} />
        <Route path="settings" element={<Settings />} />
      </Route>
      {/* 路由表外路径归位解析页（守卫仍生效，未登录先落登录） */}
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
