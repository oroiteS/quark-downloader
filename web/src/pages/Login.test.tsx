// 登录页行为钉死（简报 Step 1）：
// - start 返回 base64 PNG → img 渲染（data:image/png;base64 直用）且 alt 非空
// - state 序列 waiting→scanned→got_ticket(nickname) → 文案三段变化、轮询停止、
//   刷新 session store（setAccount 完整账户）并跳 /
// - expired 终态 → 失效文案 + 重新生成可用（重新 POST qr/start，重启轮询）
// - Cookie 导入成功 → store loggedIn=true 并跳 /
// 补充钉死：invalid/timeout 文案、轮询遇 400 停止、img 点击 restart、
// Cookie 失败显示原因不跳转、空 Cookie 前端拦截、start 失败错误态。
// 手法（简报钉死）：fake timers 驱动轮询；MemoryRouter + location 探针断言
// 跳转；api 以 vi.mock 模块级替换（qr/state 用队列按序出队）。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { Login } from './Login';
import { QR_POLL_INTERVAL_MS } from '../api/auth';
import { api } from '../api/client';
import { ApiError } from '../api/types';
import { useSession } from '../stores/session';

vi.mock('../api/client', () => ({
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn() },
}));

const PNG_BASE64 = btoa('fake-png-bytes');
const GIB = 1024 ** 3;
const ACCOUNT = {
  logged_in: true,
  nickname: '盘友甲',
  avatar: 'https://example.com/a.png',
  capacity: { total: 50 * GIB, used: 31 * GIB },
};

/** qr/state 按序出队的响应队列；耗尽后回落 waiting（永不终态）。 */
let stateQueue: Array<Record<string, unknown> | Error>;

function mockApiDefaults(): void {
  vi.mocked(api.get).mockImplementation(async (path: string) => {
    if (path === '/api/login/qr/state') {
      const next = stateQueue.shift();
      if (next instanceof Error) throw next;
      return (next ?? { state: 'waiting', logged_in: false }) as never;
    }
    if (path === '/api/account') return ACCOUNT as never;
    throw new Error(`unexpected GET ${path}`);
  });
  vi.mocked(api.post).mockImplementation(async (path: string) => {
    if (path === '/api/login/qr/start') {
      return {
        token: 'tk',
        qrcode_png: PNG_BASE64,
        scan_url: 'https://openapi.pan.baidu.com/qr',
      } as never;
    }
    if (path === '/api/login/cookie') return { logged_in: true } as never;
    throw new Error(`unexpected POST ${path}`);
  });
}

function stateCalls(): number {
  return vi.mocked(api.get).mock.calls.filter(([p]) => p === '/api/login/qr/state')
    .length;
}

function startCalls(): number {
  return vi.mocked(api.post).mock.calls.filter(([p]) => p === '/api/login/qr/start')
    .length;
}

let seen: string[] = [];
function LocationProbe() {
  const { pathname } = useLocation();
  if (seen[seen.length - 1] !== pathname) {
    seen.push(pathname);
  }
  return <span data-testid="loc">{pathname}</span>;
}

/** 渲染登录页并 flush 挂载 effect（自动 start 的 POST + 首帧状态）。 */
async function renderLogin(): Promise<void> {
  seen = [];
  render(
    <MemoryRouter initialEntries={['/login']}>
      <LocationProbe />
      <Login />
    </MemoryRouter>,
  );
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
}

/** 推进 n 个轮询周期（每周期 flush 微任务链与状态更新）。 */
async function advancePolls(count: number): Promise<void> {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(QR_POLL_INTERVAL_MS * count);
  });
}

async function expandCookiePanel(): Promise<void> {
  fireEvent.click(screen.getByText('导入 Cookie'));
}

async function submitCookie(text: string): Promise<void> {
  fireEvent.change(screen.getByLabelText('Cookie 文本'), {
    target: { value: text },
  });
  fireEvent.click(screen.getByRole('button', { name: '导入并登录' }));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
}

beforeEach(() => {
  vi.useFakeTimers();
  useSession.setState({
    loggedIn: false,
    nickname: '',
    avatar: '',
    capacity: null,
    pendingParseText: '',
  });
  stateQueue = [];
  mockApiDefaults();
});

afterEach(() => {
  cleanup();
  // 清调用历史:mock.calls 跨用例累积会污染「轮询恰 N 次停止」类计数断言
  vi.clearAllMocks();
  vi.useRealTimers();
});

describe('扫码登录', () => {
  it('start 成功：img 渲染 base64 PNG（data URL 直用）且 alt 非空，初始文案「等待扫描」', async () => {
    await renderLogin();

    const img = screen.getByRole('img');
    expect(img).toHaveAttribute('src', `data:image/png;base64,${PNG_BASE64}`);
    expect(img.getAttribute('alt')!.length).toBeGreaterThan(0);
    expect(startCalls()).toBe(1);
    expect(screen.getByText('等待扫描')).toBeInTheDocument();
    // 状态变化经 aria-live="polite" 播报（简报钉死）
    expect(screen.getByText('等待扫描')).toHaveAttribute('aria-live', 'polite');
  });

  it('state 序列 waiting→scanned→got_ticket：文案三段变化、轮询停止、刷新 store 并跳 /', async () => {
    stateQueue = [
      { state: 'waiting', logged_in: false },
      { state: 'scanned', logged_in: false },
      { state: 'got_ticket', logged_in: true, nickname: '盘友甲' },
    ];
    await renderLogin();
    // 首轮轮询未归：state=null + 二维码在手 → 兜底文案「等待扫描」
    expect(screen.getByText('等待扫描')).toBeInTheDocument();

    // 三段文案变化逐一对应三轮轮询（w → s → got_ticket）
    await advancePolls(1);
    expect(screen.getByText('等待扫描')).toBeInTheDocument();

    await advancePolls(1);
    expect(screen.getByText('已扫描待确认')).toBeInTheDocument();

    await advancePolls(1);
    expect(screen.getByText('登录成功')).toBeInTheDocument();

    // got_ticket 是终态：恰轮询 3 次后停止，再推进不再发请求
    expect(stateCalls()).toBe(3);
    await advancePolls(2);
    expect(stateCalls()).toBe(3);

    // 成功后续：拉 /api/account 按 setAccount 签名刷新 store → 跳 /
    expect(useSession.getState().loggedIn).toBe(true);
    expect(useSession.getState().nickname).toBe('盘友甲');
    expect(useSession.getState().capacity).toEqual(ACCOUNT.capacity);
    expect(screen.getByTestId('loc')).toHaveTextContent('/');
    expect(seen).toEqual(['/login', '/']);
  });

  it('expired 终态：显示「已过期」并停止轮询，重新生成可重新 start', async () => {
    stateQueue = [{ state: 'expired', logged_in: false }];
    await renderLogin();
    await advancePolls(1);

    expect(screen.getByText('已过期')).toBeInTheDocument();
    expect(stateCalls()).toBe(1);
    await advancePolls(2);
    expect(stateCalls()).toBe(1); // 终态停止

    // 终态引导重新 start：点击「重新生成二维码」→ 二次 POST + 文案复位
    fireEvent.click(screen.getByRole('button', { name: '重新生成二维码' }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(startCalls()).toBe(2);
    expect(screen.getByText('等待扫描')).toBeInTheDocument();
    expect(screen.getByRole('img')).toBeInTheDocument();
  });

  it('invalid 终态：显示「二维码失效」', async () => {
    stateQueue = [{ state: 'invalid', logged_in: false }];
    await renderLogin();
    await advancePolls(1);
    expect(screen.getByText('二维码失效')).toBeInTheDocument();
  });

  it('timeout 终态：显示「超时」', async () => {
    stateQueue = [{ state: 'timeout', logged_in: false }];
    await renderLogin();
    await advancePolls(1);
    expect(screen.getByText('超时')).toBeInTheDocument();
  });

  it('轮询遇 400（终态后会话已清）：立即停止轮询并给出重新生成入口', async () => {
    stateQueue = [
      { state: 'waiting', logged_in: false },
      new ApiError('no_qr_session', '没有进行中的扫码登录会话', 400),
    ];
    await renderLogin();
    await advancePolls(1);
    expect(screen.getByText('等待扫描')).toBeInTheDocument();

    await advancePolls(1); // 该次轮询 400
    expect(stateCalls()).toBe(2);
    await advancePolls(2);
    expect(stateCalls()).toBe(2); // 400 后停止（服务端会话已清，续轮询无意义）

    expect(
      screen.getByRole('button', { name: '重新生成二维码' }),
    ).toBeInTheDocument();
    expect(screen.getByTestId('loc')).toHaveTextContent('/login');
  });

  it('点击二维码重新 start（restart 的另一入口）', async () => {
    await renderLogin();
    expect(startCalls()).toBe(1);

    fireEvent.click(screen.getByRole('img').closest('button')!);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(startCalls()).toBe(2);
    expect(screen.getByRole('img')).toBeInTheDocument();
  });

  it('start 失败（502）：显示错误原因与重试入口，不渲染二维码', async () => {
    vi.mocked(api.post).mockImplementation(async (path: string) => {
      if (path === '/api/login/qr/start') {
        throw new ApiError('qr_start_failed', '二维码生成失败，请稍后重试', 502);
      }
      throw new Error(`unexpected POST ${path}`);
    });
    await renderLogin();

    expect(screen.queryByRole('img')).toBeNull();
    expect(screen.getByRole('alert')).toHaveTextContent(
      '二维码生成失败，请稍后重试',
    );
    expect(
      screen.getByRole('button', { name: '重新生成二维码' }),
    ).toBeInTheDocument();
  });
});

describe('Cookie 导入', () => {
  it('成功：POST {text} → 拉账户刷新 store（loggedIn=true）并跳 /', async () => {
    await renderLogin();
    await expandCookiePanel();
    await submitCookie('__pus=abc; __puus=xyz');

    expect(vi.mocked(api.post)).toHaveBeenCalledWith('/api/login/cookie', {
      text: '__pus=abc; __puus=xyz',
    });
    expect(useSession.getState().loggedIn).toBe(true);
    expect(useSession.getState().nickname).toBe('盘友甲');
    expect(screen.getByTestId('loc')).toHaveTextContent('/');
    expect(seen).toEqual(['/login', '/']);
  });

  it('失败 400：显示后端原因（role=alert），不跳转、不误写登录态', async () => {
    vi.mocked(api.post).mockImplementation(async (path: string) => {
      if (path === '/api/login/qr/start') {
        return { token: 'tk', qrcode_png: PNG_BASE64, scan_url: 'u' } as never;
      }
      if (path === '/api/login/cookie') {
        throw new ApiError('invalid_cookie', 'Cookie 无效或已过期', 400);
      }
      throw new Error(`unexpected POST ${path}`);
    });
    await renderLogin();
    await expandCookiePanel();
    await submitCookie('bad-cookie');

    expect(screen.getByRole('alert')).toHaveTextContent('Cookie 无效或已过期');
    expect(useSession.getState().loggedIn).toBe(false);
    expect(screen.getByTestId('loc')).toHaveTextContent('/login');
  });

  it('空文本提交：前端拦截，不发请求', async () => {
    await renderLogin();
    await expandCookiePanel();
    fireEvent.click(screen.getByRole('button', { name: '导入并登录' }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    expect(
      vi.mocked(api.post).mock.calls.filter(([p]) => p === '/api/login/cookie'),
    ).toHaveLength(0);
    expect(screen.getByRole('alert')).toHaveTextContent('请输入 Cookie 文本');
    expect(screen.getByTestId('loc')).toHaveTextContent('/login');
  });
});
