// 任务页行为钉死（Task 7，简报 Step 1）。词面九值（Ruling T3-F1）：简报
// 原文混合任务集「failed」系旧词残留，控制者裁决终态失败以 error 为准。
// 手法沿 T5/T6 先例：api 模块级 mock；直接操作 zustand store 注入混合任务
// 集；MemoryRouter + location 探针断言跳转；剪贴板 defineProperty mock。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import '@testing-library/jest-dom/vitest';
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { groupByBatch, Tasks } from './Tasks';
import { ToastHost } from '../components/ui';
import { api } from '../api/client';
import type { Task } from '../api/types';
import { useTasks } from '../stores/tasks';

vi.mock('../api/client', () => ({
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn() },
}));

const GIB = 1024 ** 3;
const T0 = Date.UTC(2026, 9, 12, 2, 30, 0);
const T1 = T0 + 3_600_000;

function makeTask(overrides: Partial<Task> & { id: string }): Task {
  return {
    file_name: `${overrides.id}.zip`,
    rel_dir: 'docs',
    size: 2 * GIB,
    state: 'running',
    done: GIB,
    speed_bps: 1.5 * 1024 ** 2,
    eta_ms: 90_000,
    mode: 'a',
    created_at: T0,
    ...overrides,
  };
}

/** 路由探针：记录每次渲染时的 pathname（跳转断言用）。 */
let seen: string[] = [];
function LocationProbe() {
  const location = useLocation();
  seen.push(location.pathname);
  return null;
}

function mockApi(tasks: Task[] | Error): void {
  vi.mocked(api.get).mockImplementation(async (path: string) => {
    if (path === '/api/tasks') {
      if (tasks instanceof Error) throw tasks;
      return tasks as never;
    }
    throw new Error(`unexpected GET ${path}`);
  });
  vi.mocked(api.post).mockResolvedValue({ status: 'ok' } as never);
}

/** 渲染并冲刷挂载降级 GET 的微任务（避免后到的 replaceAll 覆盖测试注入）。 */
async function renderPage(): Promise<ReturnType<typeof render>> {
  const utils = render(
    <MemoryRouter initialEntries={['/tasks']}>
      <LocationProbe />
      <Tasks />
      <ToastHost />
    </MemoryRouter>,
  );
  await act(async () => {});
  return utils;
}

function rowOf(fileName: string): HTMLElement {
  return screen.getByText(fileName).closest('li')!;
}

beforeEach(() => {
  seen = [];
  vi.mocked(api.get).mockReset();
  vi.mocked(api.post).mockReset();
  useTasks.getState().replaceAll([]);
  // jsdom 未实现剪贴板：可控 mock（写入断言用）
  Object.defineProperty(navigator, 'clipboard', {
    value: { writeText: vi.fn().mockResolvedValue(undefined) },
    configurable: true,
  });
});

afterEach(() => cleanup());

describe('任务页（SSE 未连接降级拉取与批次分组）', () => {
  it('挂载即 GET /api/tasks 一次性拉取（裸数组），经 store 渲染为行', async () => {
    mockApi([makeTask({ id: 't1' })]);
    await renderPage();
    expect(api.get).toHaveBeenCalledWith('/api/tasks');
    expect(screen.getByText('t1.zip')).toBeInTheDocument();
  });

  it('拉取失败不崩溃，弹错误 toast（SSE hello 到达后仍可对账）', async () => {
    mockApi(new Error('boom'));
    await renderPage();
    expect(await screen.findByText('任务列表加载失败')).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('任务');
  });

  it('两组 created_at → 两个分组头「批次 HH:MM · N 项」', async () => {
    mockApi([]);
    await renderPage();
    act(() => {
      useTasks.getState().replaceAll([
        makeTask({ id: 'a', created_at: T0 }),
        makeTask({ id: 'b', created_at: T0 }),
        makeTask({ id: 'c', created_at: T1 }),
      ]);
    });
    const heads = screen.getAllByRole('heading', { level: 2 });
    expect(heads).toHaveLength(2);
    expect(heads[0]).toHaveTextContent(/批次 \d{2}:\d{2} · 1 项/); // 新批次在前
    expect(heads[1]).toHaveTextContent(/批次 \d{2}:\d{2} · 2 项/);
  });

  it('空列表显示空态提示', async () => {
    mockApi([]);
    await renderPage();
    expect(screen.getByText('暂无下载任务')).toBeInTheDocument();
  });
});

describe('九词面操作集与徽章（混合任务集经 store 注入）', () => {
  it('各行按钮集与徽章正确（running/paused/error/canceled/needs_reparse/exists/done）', async () => {
    mockApi([]);
    await renderPage();
    act(() => {
      useTasks.getState().replaceAll([
        makeTask({ id: 't-run', state: 'running' }),
        makeTask({ id: 't-pause', state: 'paused' }),
        makeTask({ id: 't-err', state: 'error', error: '磁盘已满' }),
        makeTask({ id: 't-cancel', state: 'canceled' }),
        makeTask({ id: 't-reparse', state: 'needs_reparse', needs_reparse: true }),
        makeTask({ id: 't-exists', state: 'exists' }),
        makeTask({ id: 't-done', state: 'done' }),
      ]);
    });

    const run = within(rowOf('t-run.zip'));
    expect(run.getByRole('button', { name: '暂停' })).toBeInTheDocument();
    expect(run.getByRole('button', { name: '取消' })).toBeInTheDocument();
    expect(run.queryByRole('button', { name: '重试' })).not.toBeInTheDocument();
    expect(run.getByText('下载中')).toBeInTheDocument();

    const pause = within(rowOf('t-pause.zip'));
    expect(pause.getByRole('button', { name: '继续' })).toBeInTheDocument();
    expect(pause.getByRole('button', { name: '取消' })).toBeInTheDocument();
    expect(pause.queryByRole('button', { name: '暂停' })).not.toBeInTheDocument();
    expect(pause.getByText('已暂停')).toBeInTheDocument();

    const err = within(rowOf('t-err.zip'));
    expect(err.getByRole('button', { name: '重试' })).toBeInTheDocument();
    expect(err.getByRole('button', { name: '复制错误信息' })).toBeInTheDocument();
    expect(err.queryByRole('button', { name: '取消' })).not.toBeInTheDocument();
    expect(err.getByText('磁盘已满')).toBeInTheDocument();
    expect(err.getByText('失败')).toBeInTheDocument();

    const cancel = within(rowOf('t-cancel.zip'));
    expect(cancel.getByRole('button', { name: '重试' })).toBeInTheDocument();
    expect(cancel.queryByRole('button', { name: '取消' })).not.toBeInTheDocument();

    const reparse = within(rowOf('t-reparse.zip'));
    expect(reparse.getByRole('button', { name: '需重新解析分享' })).toBeInTheDocument();
    expect(reparse.queryByRole('button', { name: '重试' })).not.toBeInTheDocument();

    const exists = within(rowOf('t-exists.zip'));
    expect(exists.getByText('已存在')).toBeInTheDocument();
    expect(exists.queryAllByRole('button')).toHaveLength(0);

    const done = within(rowOf('t-done.zip'));
    expect(done.queryAllByRole('button')).toHaveLength(0);
    expect(done.getByText('已完成')).toBeInTheDocument();
  });

  it('needs_reparse 文案逐字「需重新解析分享」，无重试', async () => {
    mockApi([]);
    await renderPage();
    act(() => {
      useTasks
        .getState()
        .replaceAll([makeTask({ id: 't1', state: 'needs_reparse', needs_reparse: true })]);
    });
    expect(screen.getByText('需重新解析分享')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '重试' })).not.toBeInTheDocument();
  });

  it('needs_reparse 行可点跳解析页（/）', async () => {
    mockApi([]);
    await renderPage();
    act(() => {
      useTasks
        .getState()
        .replaceAll([makeTask({ id: 't1', state: 'needs_reparse', needs_reparse: true })]);
    });
    fireEvent.click(screen.getByRole('button', { name: '需重新解析分享' }));
    expect(seen[seen.length - 1]).toBe('/');
  });

  it('未知 state（契约违规）兜底渲染不崩溃，徽章显示原词', async () => {
    mockApi([]);
    await renderPage();
    act(() => {
      useTasks
        .getState()
        .replaceAll([makeTask({ id: 't1', state: 'weird_state' as Task['state'] })]);
    });
    expect(screen.getByText('t1.zip')).toBeInTheDocument();
    expect(screen.getByText('weird_state')).toBeInTheDocument();
  });
});

describe('进度显示', () => {
  it('进度条 role=progressbar + aria-valuenow/max（done/size 字节值）', async () => {
    mockApi([]);
    await renderPage();
    act(() => {
      useTasks.getState().replaceAll([makeTask({ id: 't1' })]);
    });
    const bar = within(rowOf('t1.zip')).getByRole('progressbar');
    expect(bar).toHaveAttribute('aria-valuemin', '0');
    expect(bar).toHaveAttribute('aria-valuenow', String(GIB));
    expect(bar).toHaveAttribute('aria-valuemax', String(2 * GIB));
  });

  it('merging=true：显示「合并中」且进度条填充置灰（修饰类）', async () => {
    mockApi([]);
    await renderPage();
    act(() => {
      useTasks.getState().replaceAll([makeTask({ id: 't1' })]);
      useTasks.getState().applyProgress('t1', {
        done: GIB,
        total: 2 * GIB,
        speed_bps: 0,
        eta_ms: 0,
        state: 'running',
        merging: true,
      });
    });
    expect(screen.getByText('合并中')).toBeInTheDocument();
    const bar = within(rowOf('t1.zip')).getByRole('progressbar');
    const fill = bar.querySelector('.task-row__bar-fill');
    expect(fill).toHaveClass('task-row__bar-fill--merging');
  });

  it('速度/ETA/大小按 format 纯函数渲染', async () => {
    mockApi([]);
    await renderPage();
    act(() => {
      useTasks.getState().replaceAll([makeTask({ id: 't1' })]);
    });
    const row = within(rowOf('t1.zip'));
    expect(row.getByText('1.50 MB/s')).toBeInTheDocument();
    expect(row.getByText('1m 30s')).toBeInTheDocument();
    expect(row.getByText('2.00 GiB')).toBeInTheDocument();
  });
});

describe('操作行为（POST 到对应 id；操作后 UI 以 store/SSE 为权威）', () => {
  it('点暂停 → POST /api/tasks/:id/pause', async () => {
    mockApi([]);
    await renderPage();
    act(() => {
      useTasks.getState().replaceAll([makeTask({ id: 't-run', state: 'running' })]);
    });
    fireEvent.click(within(rowOf('t-run.zip')).getByRole('button', { name: '暂停' }));
    await waitFor(() =>
      expect(api.post).toHaveBeenCalledWith('/api/tasks/t-run/pause'),
    );
  });

  it('点重试 → POST /api/tasks/:id/retry', async () => {
    mockApi([]);
    await renderPage();
    act(() => {
      useTasks.getState().replaceAll([makeTask({ id: 't-err', state: 'error' })]);
    });
    fireEvent.click(within(rowOf('t-err.zip')).getByRole('button', { name: '重试' }));
    await waitFor(() =>
      expect(api.post).toHaveBeenCalledWith('/api/tasks/t-err/retry'),
    );
  });

  it('复制错误信息 → clipboard.writeText(错误文本)', async () => {
    mockApi([]);
    await renderPage();
    act(() => {
      useTasks
        .getState()
        .replaceAll([makeTask({ id: 't1', state: 'error', error: '磁盘已满' })]);
    });
    fireEvent.click(screen.getByRole('button', { name: '复制错误信息' }));
    await waitFor(() =>
      expect(navigator.clipboard.writeText).toHaveBeenCalledWith('磁盘已满'),
    );
  });

  it('操作 POST 失败 → 错误 toast，不崩溃', async () => {
    mockApi([]);
    vi.mocked(api.post).mockRejectedValueOnce(new Error('boom'));
    await renderPage();
    act(() => {
      useTasks.getState().replaceAll([makeTask({ id: 't1', state: 'running' })]);
    });
    fireEvent.click(screen.getByRole('button', { name: '暂停' }));
    expect(await screen.findByText('操作失败，请重试')).toBeInTheDocument();
  });
});

describe('groupByBatch（created_at 精确值分组纯函数）', () => {
  it('批次按 created_at 降序（新在前），组内条目降序', () => {
    const groups = groupByBatch([
      makeTask({ id: 'a', created_at: T0 }),
      makeTask({ id: 'b', created_at: T0 + 1 }),
      makeTask({ id: 'd', created_at: T0 + 1 }), // 与 b 同批次：验组内稳定序
      makeTask({ id: 'c', created_at: T0 - 5 }),
    ]);
    expect(groups.map((g) => g.createdAt)).toEqual([T0 + 1, T0, T0 - 5]);
    expect(groups[0]!.items.map((t) => t.id)).toEqual(['b', 'd']);
    expect(groups[1]!.items.map((t) => t.id)).toEqual(['a']);
    expect(groups[2]!.items.map((t) => t.id)).toEqual(['c']);
  });

  it('组头 label 格式「批次 HH:MM · N 项」', () => {
    const groups = groupByBatch([
      makeTask({ id: 'a', created_at: T0 }),
      makeTask({ id: 'b', created_at: T0 }),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0]!.label).toMatch(/^批次 \d{2}:\d{2} · 2 项$/);
  });
});
