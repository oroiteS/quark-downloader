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
 * 401/网络错误一律 loggedIn=false（auth.go:342 未登录即 401 not_logged_in）。
 *
 * 页面本体归 Task 5+：简报 Files 清单不含页面文件，此处仅占位组件内联。
 * SkipLink 同理落在 App.tsx（清单未含独立文件），main.tsx 与测试共同消费。
 */
import { Navigate, Outlet, Route, Routes } from 'react-router-dom';
import type { ReactNode } from 'react';
import { api } from './api/client';
import type { Account } from './api/types';
import { useSession } from './stores/session';
import { Spinner } from './components/ui';
import { Sidebar } from './layout/Sidebar';
import './app.css';

/** 启动探测：GET /api/account 写 session store；失败视为未登录。 */
export async function probeAccount(): Promise<void> {
  try {
    const account = await api.get<Account>('/api/account');
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

/** 登录页占位（扫码登录归后续任务）；同样提供 main 地标供 skip link 落点。 */
function LoginPage() {
  return (
    <main id="main-content" className="app-login">
      <h1>登录</h1>
      <p className="page-placeholder">扫码登录页待后续任务落地。</p>
    </main>
  );
}

/** 页面占位：页面本体由 Task 5+ 按路由逐个替换。 */
function PagePlaceholder({ title }: { title: string }) {
  return (
    <section>
      <h1>{title}</h1>
      <p className="page-placeholder">页面待后续任务落地。</p>
    </section>
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
      <Route path="/login" element={<LoginPage />} />
      <Route
        element={
          <RequireAuth>
            <AppShell />
          </RequireAuth>
        }
      >
        <Route index element={<PagePlaceholder title="解析" />} />
        <Route path="tasks" element={<PagePlaceholder title="任务" />} />
        <Route path="drive" element={<PagePlaceholder title="网盘" />} />
        <Route path="settings" element={<PagePlaceholder title="设置" />} />
      </Route>
      {/* 路由表外路径归位解析页（守卫仍生效，未登录先落登录） */}
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
