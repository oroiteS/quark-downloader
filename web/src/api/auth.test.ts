// 扫码登录数据层防御钉死（真机复验三轮）：
// useQrLogin 的 got_ticket 空昵称防御（api/auth.ts:75
// `if (s.state === 'got_ticket' && s.nickname)`）——后端契约 got_ticket 必带
// nickname（auth.go 实测），空昵称属契约外形态，hook 不写入（保持 ''），
// 由 Login 页 `qr.nickname &&` 双保险不显示欢迎语；此用例钉死防回归。
// 正向链路（带昵称）一并钉死，确认防御不在正常路径误伤。
// 手法（同 Login.test.tsx）：fake timers 驱动轮询；api 以 vi.mock 模块级
// 替换（qr/state 用队列按序出队）；renderHook 驱动 hook。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  act,
  cleanup,
  renderHook,
  type RenderHookResult,
} from '@testing-library/react';
import { QR_POLL_INTERVAL_MS, useQrLogin } from './auth';
import { api } from './client';

vi.mock('./client', () => ({
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn() },
}));

const PNG_BASE64 = btoa('fake-png-bytes');

/** qr/state 按序出队的响应队列；耗尽后回落 waiting（永不终态）。 */
let stateQueue: Array<Record<string, unknown> | Error>;

function mockApiDefaults(): void {
  vi.mocked(api.post).mockImplementation(async (path: string) => {
    if (path === '/api/login/qr/start') {
      return {
        token: 'tk',
        qrcode_png: PNG_BASE64,
        scan_url: 'https://openapi.pan.quark.cn/qr',
      } as never;
    }
    throw new Error(`unexpected POST ${path}`);
  });
  vi.mocked(api.get).mockImplementation(async (path: string) => {
    if (path === '/api/login/qr/state') {
      const next = stateQueue.shift();
      if (next instanceof Error) throw next;
      return (next ?? { state: 'waiting', logged_in: false }) as never;
    }
    throw new Error(`unexpected GET ${path}`);
  });
}

/** renderHook 发起 start 并 flush 生成二维码的 POST。 */
async function renderAndStart(): Promise<
  RenderHookResult<ReturnType<typeof useQrLogin>, unknown>
> {
  const view = renderHook(() => useQrLogin());
  act(() => {
    void view.result.current.start();
  });
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  return view;
}

/** 推进 n 个轮询周期（每周期 flush 微任务链与状态更新）。 */
async function advancePolls(count: number): Promise<void> {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(QR_POLL_INTERVAL_MS * count);
  });
}

beforeEach(() => {
  vi.useFakeTimers();
  stateQueue = [];
  mockApiDefaults();
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.useRealTimers();
});

describe('useQrLogin got_ticket 空昵称防御', () => {
  it('got_ticket 带昵称：state/nickname 正常写入（正向链路不受防御误伤）', async () => {
    stateQueue = [{ state: 'got_ticket', logged_in: true, nickname: '盘友甲' }];
    const { result } = await renderAndStart();
    await advancePolls(1);

    expect(result.current.state).toBe('got_ticket');
    expect(result.current.nickname).toBe('盘友甲');
  });

  it('got_ticket 但昵称为空（契约外形态）：state 照写终态、nickname 保持空不写入', async () => {
    stateQueue = [{ state: 'got_ticket', logged_in: true, nickname: '' }];
    const { result } = await renderAndStart();
    await advancePolls(1);

    // 终态照常收敛（轮询停止、页面走 enterApp 确认链路），
    // 但空昵称不得写入 hook（Login 页欢迎语条件 qr.nickname 随之不满足）
    expect(result.current.state).toBe('got_ticket');
    expect(result.current.nickname).toBe('');
  });
});
