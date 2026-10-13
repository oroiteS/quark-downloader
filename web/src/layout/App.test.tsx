// App 壳/路由/登录守卫行为钉死（简报 Step 1）：
// - 三态守卫：null=全屏 Spinner 不跳转 / false=重定向 /login / true=放行
// - D2 导航四项 + 当前项 aria-current="page"（NavLink 内建语义）
// - AccountBadge 容量换算（wire 为字节 → 1024 基准 GiB 显示）与容量环弧度=used/total
// - 会话过期（logoutReset 置 false）→ 自动跳 /login 且不循环（location 只变一次）
// - skip link 存在且指向 #main-content
// 路由断言手法（简报钉死）：MemoryRouter + location 探针；会话过期直接操作
// useSession store；api 以 vi.mock 模块级替换。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { App, SkipLink, probeAccount } from '../App';
import { api } from '../api/client';
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
});

describe('App D2 布局', () => {
  it('loggedIn=true：导航四项可见、当前项 aria-current、AccountBadge 31/50G、容量环弧度 31/50', async () => {
    mockAccount(LOGGED_IN_ACCOUNT);
    await probeAccount();
    renderAppAt('/tasks');

    for (const label of ['解析', '任务', '网盘', '设置']) {
      expect(screen.getByRole('link', { name: label })).toBeInTheDocument();
    }
    expect(
      screen.getByRole('navigation', { name: '主导航' }),
    ).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '任务' })).toHaveAttribute(
      'aria-current',
      'page',
    );
    expect(
      screen.getByRole('link', { name: '解析' }),
    ).not.toHaveAttribute('aria-current');

    // 容量文本：wire 字节按 1024 基准换算 GiB（G 即 GiB）
    expect(screen.getByText('31/50G')).toBeInTheDocument();

    // avatar 无值 → 昵称首字符占位圈（T10：真机头像缺失修复）
    const fallback = document.querySelector(
      '.account-badge__avatar--fallback',
    );
    expect(fallback).not.toBeNull();
    expect(fallback).toHaveTextContent('n');
    expect(document.querySelector('img.account-badge__avatar')).toBeNull();

    // 容量环弧度 = used/total：offset 与 dasharray 同一周长推导；
    // 0.62 < 0.7 → 三级色第一档 primary（inline style 的 var() 字面值，
    // jsdom 不做 CSS 变量求值——断言变量引用即钉住分级选色）
    const ring = document.querySelector(
      '.capacity-ring__value',
    ) as SVGCircleElement;
    expect(ring).not.toBeNull();
    const dasharray = Number(ring.getAttribute('stroke-dasharray'));
    const dashoffset = Number(ring.getAttribute('stroke-dashoffset'));
    expect((dasharray - dashoffset) / dasharray).toBeCloseTo(31 / 50);
    expect(ring.style.stroke).toBe('var(--color-primary)');
  });

  // 容量环三级色（T10 真机复验二轮）：占用 <70% primary、70%（含）～90%
  // warning、90%（含）以上 error（ratio 钳 1，可超 100% 满环红）。
  it('容量环三级色：0.8→warning；超 100% 钳 1→error 且满环', async () => {
    mockAccount({
      ...LOGGED_IN_ACCOUNT,
      capacity: { total: 50 * GIB, used: 40 * GIB }, // 0.8 → warning 档
    });
    await probeAccount();
    renderAppAt('/tasks');

    const ring = document.querySelector(
      '.capacity-ring__value',
    ) as SVGCircleElement;
    expect(ring.style.stroke).toBe('var(--color-warning)');
    expect((Number(ring.getAttribute('stroke-dasharray')) -
      Number(ring.getAttribute('stroke-dashoffset'))) /
      Number(ring.getAttribute('stroke-dasharray'))).toBeCloseTo(0.8);
  });

  it('容量环三级色：90%（含）以上 error（超 100% 时 ratio 钳 1 满环）', async () => {
    mockAccount({
      ...LOGGED_IN_ACCOUNT,
      capacity: { total: 50 * GIB, used: 55 * GIB }, // 1.1 → 钳 1 → error
    });
    await probeAccount();
    renderAppAt('/tasks');

    const ring = document.querySelector(
      '.capacity-ring__value',
    ) as SVGCircleElement;
    expect(ring.style.stroke).toBe('var(--color-error)');
    expect(ring.getAttribute('stroke-dashoffset')).toBe('0');
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
