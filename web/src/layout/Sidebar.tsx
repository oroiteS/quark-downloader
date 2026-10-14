/**
 * D2 左侧导航（Task 4 + 2025 前端重构）：固定 nav 两项（解析/任务）
 * + 底部 AccountBadge。设置项取消（入口移至左下角用户菜单，见
 * AccountBadge）；网盘项移除（产品只保留分享链接解析下载，后端
 * /api/drive/* 接口保留未动）。NavLink 当前项自动携带 aria-current="page"
 * （读屏器播报当前位置），`/` 为前缀根必须 end 精确匹配，否则所有路由
 * 都命中解析项。导航项图标为内联 SVG（skill：装饰性 SVG aria-hidden，
 * 不进无障碍树），视觉参考整体框架图（图标 + 文字的侧栏项）。
 */
import { NavLink } from 'react-router-dom';
import type { ReactNode } from 'react';
import { AccountBadge } from './AccountBadge';

/** 解析（链接/分享链）：feather link，stroke 继承 currentColor。 */
function ParseIcon() {
  return (
    <svg
      viewBox="0 0 24 24"
      width={18}
      height={18}
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      <path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71" />
      <path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71" />
    </svg>
  );
}

/** 任务（下载）：feather download。 */
function TasksIcon() {
  return (
    <svg
      viewBox="0 0 24 24"
      width={18}
      height={18}
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
      <polyline points="7 10 12 15 17 10" />
      <line x1="12" y1="15" x2="12" y2="3" />
    </svg>
  );
}

const NAV_ITEMS: { to: string; label: string; end: boolean; icon: ReactNode }[] =
  [
    { to: '/', label: '解析', end: true, icon: <ParseIcon /> },
    { to: '/tasks', label: '任务', end: false, icon: <TasksIcon /> },
  ];

export function Sidebar() {
  return (
    <div className="sidebar">
      {/* nav 语义仅覆盖导航链接列表；AccountBadge（用户区）在 nav 外，
          避免「用户菜单」被读屏器当作主导航内容播报 */}
      <nav className="sidebar__nav-wrap" aria-label="主导航">
        <ul className="sidebar__nav">
          {NAV_ITEMS.map(({ to, label, end, icon }) => (
            <li key={to}>
              <NavLink
                to={to}
                end={end}
                className={({ isActive }) =>
                  `sidebar__link${isActive ? ' sidebar__link--active' : ''}`
                }
              >
                {icon}
                {label}
              </NavLink>
            </li>
          ))}
        </ul>
      </nav>
      <AccountBadge />
    </div>
  );
}
