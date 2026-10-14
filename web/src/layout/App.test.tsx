// App 壳/路由/登录守卫行为钉死（简报 Step 1 + 2025 前端重构）：
// - 三态守卫：null=全屏 Spinner 不跳转 / false=重定向 /login / true=放行
// - 登录态与昵称解耦（T15，校验保守原则）：200 {logged_in:true, nickname:""}
//   仍视为已登录，昵称/容量照常写入，AccountBadge「夸克用户」占位兜底
// - D2 导航两项（解析/任务；设置入口在左下角用户菜单，网盘页已移除——
//   只保留分享链接解析下载，后端 /api/drive/* 接口保留不动）
//   + 当前项 aria-current="page"（NavLink 内建语义）
// - AccountBadge 长条进度条（环已废弃）：宽度=used/total、<100% 蓝
//   （primary）、≥100% 红（error 且占满）；"used/totalG" 文本在条下方
// - 左下角昵称/头像框点击 → 弹出菜单（设置/退出登录）：设置跳 /settings；
//   退出登录 Dialog 确认 → POST /api/logout → session 清 → 跳 /login；
//   失败 toast error 停留 Dialog；Esc 关闭菜单
// - 会话过期（logoutReset 置 false）→ 自动跳 /login 且不循环（location 只变一次）
// - skip link 存在且指向 #main-content
// 路由断言手法（简报钉死）：MemoryRouter + location 探针；会话过期直接操作
// useSession store；api 以 vi.mock 模块级替换。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { App, SkipLink, probeAccount } from '../App';
import { api } from '../api/client';
import { toast } from '../components/ui';
import { useSession } from '../stores/session';

const GIB = 1024 ** 3;

vi.mock('../api/client', () => ({
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn() },
}));

// location 探针：按序记录 pathname 变化（连续重复去重）——“只变一次”的断言依据。
let seen: string[] = [];
function LocationProbe() {
  const { pathname } = useLocation();
  if (seen[seen.length - 1] !== pathname) {
    seen.push(pathname);
  }
  return <span data-testid="loc">{pathname}</span>;
}

function renderAppAt(path: string): void {
  seen = [];
  // 组合顺序与 main.tsx 一致：SkipLink 先于 App 挂载（首个可聚焦元素），
  // 仅 Router 换成 MemoryRouter（简报钉死的路由测试手法）。
  render(
    <MemoryRouter initialEntries={[path]}>
      <LocationProbe />
      <SkipLink />
      <App />
    </MemoryRouter>,
  );
}

function mockAccount(account: Record<string, unknown>): void {
  vi.mocked(api.get).mockResolvedValue(account as never);
}

const LOGGED_IN_ACCOUNT = {
  logged_in: true,
  nickname: 'n',
  avatar: '',
  capacity: { total: 50 * GIB, used: 31 * GIB }, // wire 为字节；换算后 31/50 GiB
};

beforeEach(() => {
  useSession.setState({
    loggedIn: null,
    nickname: '',
    avatar: '',
    capacity: null,
    pendingParseText: '',
  });
  vi.mocked(api.get).mockReset();
  vi.mocked(api.post).mockReset();
  vi.mocked(api.post).mockResolvedValue({ logged_in: false } as never);
  vi.spyOn(toast, 'show').mockImplementation(() => {});
});

afterEach(cleanup);

describe('App 登录守卫（三态）', () => {
  it('探测 loggedIn=false：渲染 /tasks 落地 /login', async () => {
    mockAccount({
      logged_in: false,
      nickname: '',
      avatar: '',
      capacity: { total: 0, used: 0 },
    });
    await probeAccount();
    expect(useSession.getState().loggedIn).toBe(false);

    renderAppAt('/tasks');
    expect(screen.getByTestId('loc')).toHaveTextContent('/login');
    expect(screen.getByRole('heading', { name: '登录' })).toBeInTheDocument();
  });

  it('loggedIn=null（探测中）：全屏 Spinner 不跳转', () => {
    // 不调用 probeAccount：store 保持初始 null，守卫必须原地等待而非跳转
    renderAppAt('/tasks');
    expect(screen.getByRole('status')).toBeInTheDocument();
    expect(screen.getByTestId('loc')).toHaveTextContent('/tasks');
  });

  it('探测失败（网络/500）：loggedIn=false（与 401 同语义）', async () => {
    vi.mocked(api.get).mockRejectedValue(new Error('HTTP 500'));
    await probeAccount();
    expect(useSession.getState().loggedIn).toBe(false);
  });

  // 空昵称=有效登录态（T15 解耦，校验保守原则，真机实锤）：后端回退链
  // （进程内 cachedAccount → account.json）也拿不到昵称时返回 200
  // {logged_in:true, nickname:"", capacity:{真值}}——凭据有效仅昵称拿不到。
  // 一律 setAccount（昵称空、容量照常写入），放行内页；AccountBadge 以
  // 「夸克用户」占位，「未登录」字样不出现。撤销上轮 e538df7 的误踢。
  it('探测 200 但昵称为空（回退链零值）：loggedIn=true、容量写入、AccountBadge 占位「夸克用户」', async () => {
    mockAccount({
      logged_in: true,
      nickname: '',
      avatar: '',
      capacity: { total: 24 * GIB, used: 20 * GIB }, // 真机截图：容量 24/20G 真值
    });
    await probeAccount();
    expect(useSession.getState().loggedIn).toBe(true);
    expect(useSession.getState().nickname).toBe('');
    expect(useSession.getState().capacity).toEqual({
      total: 24 * GIB,
      used: 20 * GIB,
    });

    renderAppAt('/tasks');
    expect(screen.getByText('夸克用户')).toBeInTheDocument();
    expect(screen.queryByText('未登录')).not.toBeInTheDocument();

    // 头像占位圈：空昵称取占位名首字符「夸」
    const fallback = document.querySelector(
      '.account-badge__avatar--fallback',
    );
    expect(fallback).toHaveTextContent('夸');
  });
});

describe('App D2 布局', () => {
  it('loggedIn=true：导航两项可见（无网盘/设置项）、当前项 aria-current、31/50G 文本在进度条下方', async () => {
    mockAccount(LOGGED_IN_ACCOUNT);
    await probeAccount();
    renderAppAt('/tasks');

    const nav = screen.getByRole('navigation', { name: '主导航' });
    for (const label of ['解析', '任务']) {
      expect(
        within(nav).getByRole('link', { name: label }),
      ).toBeInTheDocument();
    }
    // 网盘页已移除（只保留分享解析下载）；设置在左下角用户菜单（非链接）
    expect(within(nav).queryByRole('link', { name: '网盘' })).toBeNull();
    expect(within(nav).queryByRole('link', { name: '设置' })).toBeNull();
    expect(screen.queryByRole('link', { name: '设置' })).toBeNull();

    expect(
      within(nav).getByRole('link', { name: '任务' }),
    ).toHaveAttribute('aria-current', 'page');
    expect(
      within(nav).getByRole('link', { name: '解析' }),
    ).not.toHaveAttribute('aria-current');

    // 容量文本：wire 字节按 1024 基准换算 GiB（G 即 GiB），位于进度条下方
    expect(screen.getByText('31/50G')).toBeInTheDocument();

    // avatar 无值 → 昵称首字符占位圈（T10：真机头像缺失修复）
    const fallback = document.querySelector(
      '.account-badge__avatar--fallback',
    );
    expect(fallback).not.toBeNull();
    expect(fallback).toHaveTextContent('n');
    expect(document.querySelector('img.account-badge__avatar')).toBeNull();

    // 环已废弃：DOM 中不存在容量环
    expect(document.querySelector('.capacity-ring')).toBeNull();
  });

  // 长条进度条（2025 前端重构，环 → 长条）：宽度=used/total；
  // <100% 蓝（primary）；≥100% 红（error）且占满（ratio 钳 1）。
  it('进度条宽度=31/50、<100% 为蓝色（primary）', async () => {
    mockAccount(LOGGED_IN_ACCOUNT);
    await probeAccount();
    renderAppAt('/tasks');

    const bar = document.querySelector('.capacity-bar') as HTMLElement;
    expect(bar).not.toBeNull();
    expect(bar).toHaveAttribute('aria-hidden', 'true'); // 装饰，数值由文本承载
    const fill = document.querySelector(
      '.capacity-bar__fill',
    ) as HTMLElement;
    expect(fill).not.toBeNull();
    expect(fill.style.width).toBe('62%');
    // inline style 的 var() 字面值（jsdom 不求值 CSS 变量）——钉住两级选色
    expect(fill.style.background).toBe('var(--color-primary)');
  });

  it('进度条 0.8 仍为蓝色（原三级色废弃，仅两档）', async () => {
    mockAccount({
      ...LOGGED_IN_ACCOUNT,
      capacity: { total: 50 * GIB, used: 40 * GIB },
    });
    await probeAccount();
    renderAppAt('/tasks');

    const fill = document.querySelector(
      '.capacity-bar__fill',
    ) as HTMLElement;
    expect(fill.style.width).toBe('80%');
    expect(fill.style.background).toBe('var(--color-primary)');
  });

  it('进度条超 100%（55/50）：钳 1 占满整根且变红（error）', async () => {
    mockAccount({
      ...LOGGED_IN_ACCOUNT,
      capacity: { total: 50 * GIB, used: 55 * GIB },
    });
    await probeAccount();
    renderAppAt('/tasks');

    const fill = document.querySelector(
      '.capacity-bar__fill',
    ) as HTMLElement;
    expect(fill.style.width).toBe('100%');
    expect(fill.style.background).toBe('var(--color-error)');
  });

  it('avatar 有值：32px 圆形 img 渲染（alt="" 装饰，昵称承载语义）', async () => {
    mockAccount({ ...LOGGED_IN_ACCOUNT, avatar: 'https://example.com/a.png' });
    await probeAccount();
    renderAppAt('/tasks');

    const img = document.querySelector(
      'img.account-badge__avatar',
    ) as HTMLImageElement;
    expect(img).not.toBeNull();
    expect(img).toHaveAttribute('src', 'https://example.com/a.png');
    expect(img).toHaveAttribute('alt', '');
    expect(img).toHaveAttribute('width', '32');
    expect(img).toHaveAttribute('height', '32');
    expect(
      document.querySelector('.account-badge__avatar--fallback'),
    ).toBeNull();
  });

  it('img 加载失败：回退昵称首字符占位圈', async () => {
    mockAccount({ ...LOGGED_IN_ACCOUNT, avatar: 'https://example.com/broken.png' });
    await probeAccount();
    renderAppAt('/tasks');

    const img = document.querySelector(
      'img.account-badge__avatar',
    ) as HTMLImageElement;
    expect(img).not.toBeNull();
    fireEvent.error(img);

    const fallback = document.querySelector(
      '.account-badge__avatar--fallback',
    );
    expect(fallback).not.toBeNull();
    expect(fallback).toHaveTextContent('n');
    expect(document.querySelector('img.account-badge__avatar')).toBeNull();
  });

  it('会话过期置 false：自动跳 /login 且不循环（location 只变一次）', async () => {
    mockAccount(LOGGED_IN_ACCOUNT);
    await probeAccount();
    renderAppAt('/tasks');

    useSession.getState().logoutReset();

    await waitFor(() =>
      expect(screen.getByTestId('loc')).toHaveTextContent('/login'),
    );
    // 序列恰为 [/tasks, /login]：一次变更、无回弹循环（/login 无守卫不反弹）
    expect(seen).toEqual(['/tasks', '/login']);
    expect(screen.getByRole('heading', { name: '登录' })).toBeInTheDocument();
  });

  it('skip link 存在且指向 #main-content', async () => {
    mockAccount(LOGGED_IN_ACCOUNT);
    await probeAccount();
    renderAppAt('/');

    const skip = screen.getByRole('link', { name: '跳到主内容' });
    expect(skip).toHaveAttribute('href', '#main-content');
    expect(screen.getByRole('main')).toHaveAttribute('id', 'main-content');
  });
});

describe('左下角用户菜单（设置入口迁移 + 退出登录）', () => {
  it('初始关闭；点击昵称/头像框 → role=menu 弹出，含设置/退出登录两个 menuitem', async () => {
    mockAccount(LOGGED_IN_ACCOUNT);
    await probeAccount();
    renderAppAt('/tasks');

    expect(screen.queryByRole('menu')).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: 'n' }));

    const menu = screen.getByRole('menu');
    expect(menu).toHaveAttribute('aria-label', '用户菜单');
    expect(
      within(menu).getByRole('menuitem', { name: '设置' }),
    ).toBeInTheDocument();
    expect(
      within(menu).getByRole('menuitem', { name: '退出登录' }),
    ).toBeInTheDocument();
    // aria-expanded 响应式跟随
    expect(screen.getByRole('button', { name: 'n' })).toHaveAttribute(
      'aria-expanded',
      'true',
    );
  });

  it('点击设置 menuitem → 跳 /settings 且菜单关闭', async () => {
    mockAccount(LOGGED_IN_ACCOUNT);
    await probeAccount();
    renderAppAt('/tasks');

    fireEvent.click(screen.getByRole('button', { name: 'n' }));
    fireEvent.click(within(screen.getByRole('menu')).getByRole('menuitem', { name: '设置' }));

    await waitFor(() =>
      expect(screen.getByTestId('loc')).toHaveTextContent('/settings'),
    );
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it('Esc 关闭菜单', async () => {
    mockAccount(LOGGED_IN_ACCOUNT);
    await probeAccount();
    renderAppAt('/tasks');

    fireEvent.click(screen.getByRole('button', { name: 'n' }));
    expect(screen.getByRole('menu')).toBeInTheDocument();

    fireEvent.keyDown(screen.getByRole('menu'), { key: 'Escape' });
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it('退出登录：Dialog 确认 → POST /api/logout → session 清 → 跳 /login', async () => {
    mockAccount(LOGGED_IN_ACCOUNT);
    await probeAccount();
    renderAppAt('/tasks');

    fireEvent.click(screen.getByRole('button', { name: 'n' }));
    fireEvent.click(
      within(screen.getByRole('menu')).getByRole('menuitem', {
        name: '退出登录',
      }),
    );

    const dialog = await screen.findByRole('dialog', { name: '退出登录' });
    expect(dialog).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: '确认退出' }));
    await flush();

    expect(vi.mocked(api.post)).toHaveBeenCalledWith('/api/logout');
    const session = useSession.getState();
    expect(session.loggedIn).toBe(false);
    expect(session.nickname).toBe('');
    await waitFor(() =>
      expect(screen.getByTestId('loc')).toHaveTextContent('/login'),
    );
    // 序列恰为 [/tasks, /login]：logoutReset 置 false 后守卫跳转即到位
    expect(seen).toEqual(['/tasks', '/login']);
  });

  it('退出登录失败：toast error 后端文案，Dialog 保留可重试且不跳转', async () => {
    mockAccount(LOGGED_IN_ACCOUNT);
    await probeAccount();
    const { ApiError } = await import('../api/types');
    vi.mocked(api.post).mockRejectedValue(
      new ApiError('internal', '服务不可用', 500),
    );
    renderAppAt('/tasks');

    fireEvent.click(screen.getByRole('button', { name: 'n' }));
    fireEvent.click(
      within(screen.getByRole('menu')).getByRole('menuitem', {
        name: '退出登录',
      }),
    );
    await screen.findByRole('dialog');

    fireEvent.click(screen.getByRole('button', { name: '确认退出' }));
    await flush();

    expect(toast.show).toHaveBeenCalledWith('服务不可用', 'error');
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(screen.getByTestId('loc')).toHaveTextContent('/tasks');
  });
});

/** flush 异步状态更新（登出的微任务链，Settings.test 同型）。 */
async function flush(): Promise<void> {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}
