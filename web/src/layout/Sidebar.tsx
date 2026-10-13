/**
 * D2 左侧导航（Task 4）：固定 nav 四项（解析/任务/网盘/设置）+ 左下
 * AccountBadge。NavLink 当前项自动携带 aria-current="page"（读屏器播报
 * 当前位置），`/` 为前缀根必须 end 精确匹配，否则所有路由都命中解析项。
 */
import { NavLink } from 'react-router-dom';
import { AccountBadge } from './AccountBadge';

const NAV_ITEMS = [
  { to: '/', label: '解析', end: true },
  { to: '/tasks', label: '任务', end: false },
  { to: '/drive', label: '网盘', end: false },
  { to: '/settings', label: '设置', end: false },
] as const;

export function Sidebar() {
  return (
    <nav className="sidebar" aria-label="主导航">
      <ul className="sidebar__nav">
        {NAV_ITEMS.map(({ to, label, end }) => (
          <li key={to}>
            <NavLink
              to={to}
              end={end}
              className={({ isActive }) =>
                `sidebar__link${isActive ? ' sidebar__link--active' : ''}`
              }
            >
              {label}
            </NavLink>
          </li>
        ))}
      </ul>
      <AccountBadge />
    </nav>
  );
}
