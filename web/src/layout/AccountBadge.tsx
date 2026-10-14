/**
 * AccountBadge（D2 左下 + 2025 前端重构）：头像 + 昵称触发框，下方长条
 * 容量进度条与 "used/totalG" 文本；点击触发框弹出用户菜单（设置/退出登录）。
 *
 * - 容量 wire 单位为字节（Quark member 接口 int64 直通，protocol/api.go
 *   Capacity），按 1024 基准换算 GiB 显示——“G”即 GiB（简报钉死）。
 * - 容量呈现由「环」改「长条」（2025 前端重构，用户需求）：进度条位于
 *   昵称/头像下方，"used/totalG" 文本位于进度条下方。两档色：<100% 蓝
 *   （primary，未占满整根）、≥100%（含超限，ratio 钳 1）红（error 且占满
 *   整根）——原三级色（70%/90% 分档）随环一并废弃。宽度/颜色走 inline
 *   style（T10 先例：var() 在 SVG attribute 不生效，但 CSS 属性 inline
 *   style 的 var() 合法；类规则被改写时 inline 仍兜底跟随 token）。
 * - 条 aria-hidden（数值已由文本承载，skill：装饰性图形不进无障碍树）。
 * - 头像（T10 真机验收③）：avatar 有值→32px 圆形 img；无值或加载失败→
 *   昵称首字符占位圈。img alt=""（装饰——昵称已承载语义）；占位圈同理
 *   aria-hidden（同时保证触发框可及名恰为昵称，无重复拼接）。
 * - 昵称占位（T15 登录态与昵称解耦）：登录态下后端回退链也拿不到昵称时
 *   显示「夸克用户」占位——「未登录」字样仅 loggedIn=false 出现。
 * - 用户菜单（2025 前端重构，用户需求）：取消导航栏「设置」项，点击左下
 *   角昵称/头像框弹出菜单，含「设置」（跳 /settings）与「退出登录」两个
 *   menuitem。WAI-ARIA menu 模式：触发框 aria-haspopup/aria-expanded；
 *   Esc 关闭并归还焦点至触发框；点击外部（pointerdown 落在根外）关闭；
 *   打开即焦点入首个 menuitem。菜单锚定触发框上方（侧栏底部，向上展开）。
 * - 退出登录（自 Settings 页迁移，交互同型）：Dialog 确认 → POST
 *   /api/logout → logoutReset() 清会话 → navigate('/login')（守卫单通道
 *   语义不破坏——该通道仅约束会话过期，登出属页面业务路径）。失败 toast
 *   error、Dialog 保留可重试。
 */
import { useEffect, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { api } from '../api/client';
import { ApiError } from '../api/types';
import { Dialog, Button, toast } from '../components/ui';
import { useSession } from '../stores/session';

const GIB = 1024 ** 3;

function toGiB(bytes: number): number {
  return Math.round(bytes / GIB);
}

/** 设置齿轮（Material filled，装饰 aria-hidden）。 */
function GearIcon() {
  return (
    <svg viewBox="0 0 24 24" width={18} height={18} aria-hidden="true" focusable="false">
      <path
        fill="currentColor"
        d="M19.14 12.94c.04-.3.06-.61.06-.94 0-.32-.02-.64-.07-.94l2.03-1.58a.49.49 0 0 0 .12-.61l-1.92-3.32a.49.49 0 0 0-.59-.22l-2.39.96c-.5-.38-1.03-.7-1.62-.94l-.36-2.54a.484.484 0 0 0-.48-.41h-3.84c-.24 0-.43.17-.47.41l-.36 2.54c-.59.24-1.13.57-1.62.94l-2.39-.96a.49.49 0 0 0-.59.22L2.74 8.87c-.12.21-.08.47.12.61l2.03 1.58c-.05.3-.09.63-.09.94s.02.64.07.94l-2.03 1.58a.49.49 0 0 0-.12.61l1.92 3.32c.12.22.37.29.59.22l2.39-.96c.5.38 1.03.7 1.62.94l.36 2.54c.05.24.24.41.48.41h3.84c.24 0 .44-.17.47-.41l.36-2.54c.59-.24 1.13-.56 1.62-.94l2.39.96c.22.08.47 0 .59-.22l1.92-3.32a.49.49 0 0 0-.12-.61l-2.01-1.58zM12 15.6c-1.98 0-3.6-1.62-3.6-3.6s1.62-3.6 3.6-3.6 3.6 1.62 3.6 3.6-1.62 3.6-3.6 3.6z"
      />
    </svg>
  );
}

/** 退出登录（Material logout：左括号 + 右向箭头，装饰 aria-hidden）。 */
function LogoutIcon() {
  return (
    <svg viewBox="0 0 24 24" width={18} height={18} aria-hidden="true" focusable="false">
      <path
        fill="currentColor"
        d="M17 7l-1.41 1.41L18.17 11H8v2h10.17l-2.58 2.58L17 17l5-5zM4 5h8V3H4c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h8v-2H4V5z"
      />
    </svg>
  );
}

export function AccountBadge() {
  const loggedIn = useSession((s) => s.loggedIn);
  const nickname = useSession((s) => s.nickname);
  const avatar = useSession((s) => s.avatar);
  const capacity = useSession((s) => s.capacity);
  const [imgFailed, setImgFailed] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const [logoutOpen, setLogoutOpen] = useState(false);
  const [logoutBusy, setLogoutBusy] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const navigate = useNavigate();

  // 昵称占位（T15）：空昵称按登录态分流——已登录「夸克用户」、未登录（含
  // 探测中 loggedIn=null，壳层尚不渲染本组件，纯防御）保持「未登录」。
  const displayName = nickname || (loggedIn ? '夸克用户' : '未登录');

  // avatar 更新（重新登录/换号）时重置失败态，允许新图重试
  useEffect(() => {
    setImgFailed(false);
  }, [avatar]);

  // 菜单开起期间：Esc 关闭（焦点归还触发框）+ 点击外部关闭（WAI-ARIA menu）
  useEffect(() => {
    if (!menuOpen) return;
    function onPointerDown(e: PointerEvent): void {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        setMenuOpen(false);
      }
    }
    function onKeyDown(e: KeyboardEvent): void {
      if (e.key === 'Escape') {
        setMenuOpen(false);
        triggerRef.current?.focus();
      }
    }
    document.addEventListener('pointerdown', onPointerDown);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('pointerdown', onPointerDown);
      document.removeEventListener('keydown', onKeyDown);
    };
  }, [menuOpen]);

  // 打开即焦点入首个 menuitem（键盘用户直达菜单项）
  useEffect(() => {
    if (!menuOpen) return;
    menuRef.current?.querySelector<HTMLButtonElement>('button')?.focus();
  }, [menuOpen]);

  const used = capacity?.used ?? 0;
  const total = capacity?.total ?? 0;
  // 两档色（2025 前端重构）：<100% 蓝（primary，按占比填充未满）；
  // ≥100%（含超限，钳 1）红（error）且占满整根。
  const ratio = total > 0 ? Math.min(used / total, 1) : 0;
  const barColor = ratio >= 1 ? 'var(--color-error)' : 'var(--color-primary)';
  const showImg = avatar !== '' && !imgFailed;

  function toggleMenu(): void {
    setMenuOpen((v) => !v);
  }

  function goSettings(): void {
    setMenuOpen(false);
    navigate('/settings');
  }

  function askLogout(): void {
    setMenuOpen(false);
    setLogoutOpen(true);
  }

  async function handleLogout(): Promise<void> {
    setLogoutBusy(true);
    try {
      await api.post('/api/logout');
      useSession.getState().logoutReset();
      navigate('/login', { replace: true });
    } catch (e) {
      toast.show(
        e instanceof ApiError ? e.message : '退出登录失败，请稍后重试',
        'error',
      );
      setLogoutBusy(false); // Dialog 保留可重试
    }
  }

  return (
    <div className="account-badge" ref={rootRef}>
      <button
        ref={triggerRef}
        type="button"
        className="account-badge__trigger"
        aria-haspopup="menu"
        aria-expanded={menuOpen}
        onClick={toggleMenu}
      >
        {showImg ? (
          <img
            className="account-badge__avatar"
            src={avatar}
            alt=""
            width={32}
            height={32}
            onError={() => setImgFailed(true)}
          />
        ) : (
          <span
            className="account-badge__avatar account-badge__avatar--fallback"
            aria-hidden="true"
          >
            {displayName.trim().charAt(0)}
          </span>
        )}
        <span className="account-badge__name">{displayName}</span>
      </button>

      {capacity && (
        <div className="account-badge__usage">
          <div className="capacity-bar" aria-hidden="true">
            <div
              className="capacity-bar__fill"
              style={{ width: `${ratio * 100}%`, background: barColor }}
            />
          </div>
          <span className="account-badge__capacity">
            {toGiB(used)}/{toGiB(total)}G
          </span>
        </div>
      )}

      {menuOpen && (
        <div
          ref={menuRef}
          className="user-menu"
          role="menu"
          aria-label="用户菜单"
        >
          <button
            type="button"
            role="menuitem"
            className="user-menu__item"
            onClick={goSettings}
          >
            <GearIcon />
            设置
          </button>
          <button
            type="button"
            role="menuitem"
            className="user-menu__item"
            onClick={askLogout}
          >
            <LogoutIcon />
            退出登录
          </button>
        </div>
      )}

      <Dialog
        open={logoutOpen}
        onClose={() => setLogoutOpen(false)}
        title="退出登录"
      >
        <form
          className="account-badge__dialog-form"
          onSubmit={(e) => {
            e.preventDefault();
            void handleLogout();
          }}
        >
          <p>确定要退出当前账号吗？</p>
          <div className="account-badge__dialog-actions">
            <Button
              type="button"
              variant="secondary"
              onClick={() => setLogoutOpen(false)}
            >
              取消
            </Button>
            <Button type="submit" loading={logoutBusy}>
              确认退出
            </Button>
          </div>
        </form>
      </Dialog>
    </div>
  );
}
