// 网盘页行为钉死（简报 Step 1 四场景 + 补充边界）：
// 1. 进入渲染根列表：挂载即 GET /api/drive/files?pdir_fid=0 并渲染表格
// 2. 目录点击 → GET pdir_fid 传参正确 + 面包屑栈（根→子目录，aria-current）；
//    面包屑回根重取 pdir_fid=0
// 3. 勾选 2 文件下载 → POST /api/tasks {source:'drive', fids, dest, mode}
//    （mode 默认 a；radio 选 B（边下边播）→ mode:'b'）
// 4. mock 409 dest_busy → Toast 文案取后端 message（简报钉死）
// 补充钉住：skipped 逐条 toast（创建链 dest_busy 进 skipped 同样用户可见，
// tasks.go:307 实测）、提交中按钮禁用防重复提交、提交成功关 Dialog 清勾选、
// 拉取 502 → role=alert 后端文案、表格可及名可定制（Ruling T6-M3 tableLabel）。
// 手法（与 Parse.test 同型）：api 以 vi.mock 模块级替换；toast 以 spyOn 观测。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { Drive } from './Drive';
import { api } from '../api/client';
import { ApiError } from '../api/types';
import { toast } from '../components/ui';

vi.mock('../api/client', () => ({
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn() },
}));

// ---- 夹具（与后端实测响应形状逐字一致）----
const FILE_A = { fid: 'f1', file_name: '报告.pdf', size: 2 * 1024 ** 3, dir: false, share_fid_token: 't1' };
const DIR_B = { fid: 'd1', file_name: '剧集', size: 0, dir: true, share_fid_token: 't2' };
const FILE_C = { fid: 'f2', file_name: 'README.md', size: 3 * 1024 ** 3, dir: false, share_fid_token: 't3' };
const SUB_FILE = { fid: 'f3', file_name: '第01集.mkv', size: 4 * 1024 ** 3, dir: false, share_fid_token: 't4' };

/** 网盘目录表：pdir_fid → files（'0' 为根，后端钉死根 fid）。 */
let dirTable: Record<string, unknown[]>;
/** 记录 drive/files 实际请求过的 pdir_fid 序列。 */
let filesLog: string[];
/** 可配置的 POST /api/tasks 响应（409/skipped 用例覆写）。 */
let taskResponse: unknown;

function mockApiDefaults(): void {
  vi.mocked(api.get).mockImplementation(async (path: string) => {
    if (path.startsWith('/api/drive/files')) {
      const pdir = new URLSearchParams(path.split('?')[1]).get('pdir_fid') ?? '';
      filesLog.push(pdir);
      return { files: dirTable[pdir] ?? [] } as never;
    }
    throw new Error(`unexpected GET ${path}`);
  });
  vi.mocked(api.post).mockImplementation(async (path: string) => {
    if (path === '/api/tasks') {
      const res = taskResponse;
      if (res instanceof ApiError) throw res;
      return res as never;
    }
    throw new Error(`unexpected POST ${path}`);
  });
}

/** flush 异步状态更新（挂载拉取/建任务后的微任务链）。 */
async function flush(): Promise<void> {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

async function renderDrive(): Promise<void> {
  render(<Drive />);
  await flush();
}

function taskCall(): { source: string; fids: string[]; dest: string; mode: string } {
  const call = vi.mocked(api.post).mock.calls.find(([p]) => p === '/api/tasks');
  expect(call).toBeDefined();
  return call![1] as { source: string; fids: string[]; dest: string; mode: string };
}

/** 勾选两文件并打开下载 Dialog。 */
async function prepareChecked(): Promise<void> {
  await renderDrive();
  fireEvent.click(screen.getByRole('checkbox', { name: '选择 报告.pdf' }));
  fireEvent.click(screen.getByRole('checkbox', { name: '选择 README.md' }));
  fireEvent.click(screen.getByRole('button', { name: '下载选中' }));
  await screen.findByRole('dialog');
}

let toastShow: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  dirTable = { '0': [FILE_A, DIR_B, FILE_C], d1: [SUB_FILE] };
  filesLog = [];
  taskResponse = { task_ids: ['t-1', 't-2'], skipped: [] };
  toastShow = vi.spyOn(toast, 'show');
  mockApiDefaults();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('网盘页：根列表渲染（场景 1）', () => {
  it('进入页面：挂载即 GET drive/files?pdir_fid=0，渲染根列表（可及名按 T6-M3 可定制）', async () => {
    await renderDrive();

    expect(filesLog).toEqual(['0']);
    expect(vi.mocked(api.get).mock.calls[0][0]).toBe('/api/drive/files?pdir_fid=0');
    expect(screen.getByRole('table', { name: '网盘文件列表' })).toBeInTheDocument();
    expect(screen.getByText('报告.pdf')).toBeInTheDocument();
    expect(screen.getByText('剧集')).toBeInTheDocument();
    expect(screen.getByText('2.00 GiB')).toBeInTheDocument();
  });

  it('拉取 502（drive_list_failed）：role=alert 显示后端文案，不崩溃', async () => {
    vi.mocked(api.get).mockRejectedValue(
      new ApiError('drive_list_failed', '拉取网盘目录失败: upstream boom', 502),
    );
    render(<Drive />);

    expect(await screen.findByRole('alert')).toHaveTextContent(
      '拉取网盘目录失败: upstream boom',
    );
  });
});

describe('网盘页：目录导航与面包屑（场景 2）', () => {
  it('目录行点击：GET pdir_fid=子目录 fid，渲染子列表；面包屑栈根→子目录，当前层 aria-current；点根回退重取', async () => {
    await renderDrive();
    fireEvent.click(screen.getByRole('button', { name: '剧集' }));
    await screen.findByText('第01集.mkv');

    expect(filesLog).toEqual(['0', 'd1']);
    expect(vi.mocked(api.get).mock.calls[1][0]).toBe('/api/drive/files?pdir_fid=d1');
    expect(screen.getByText('4.00 GiB')).toBeInTheDocument();

    // 面包屑：根 + 当前目录；当前目录 aria-current 标记
    const crumbs = screen.getByRole('navigation', { name: '目录路径' });
    expect(crumbs).toHaveTextContent('网盘根目录');
    expect(crumbs).toHaveTextContent('剧集');
    expect(screen.getByText('剧集')).toHaveAttribute('aria-current', 'location');

    // 点根面包屑 → 回根列表（pdir_fid=0 重取）
    fireEvent.click(screen.getByRole('button', { name: '网盘根目录' }));
    await screen.findByText('报告.pdf');
    expect(filesLog).toEqual(['0', 'd1', '0']);
    expect(screen.queryByText('第01集.mkv')).not.toBeInTheDocument();
  });
});

describe('网盘页：勾选下载（场景 3 + mode）', () => {
  it('未勾选时下载按钮禁用', async () => {
    await renderDrive();

    expect(screen.getByRole('button', { name: '下载选中' })).toBeDisabled();
  });

  it('勾选 2 文件：POST /api/tasks {source:"drive", fids, dest:"", mode:"a"}（默认 a）', async () => {
    await prepareChecked();
    fireEvent.click(screen.getByRole('button', { name: '开始下载' }));
    await flush();

    const body = taskCall();
    expect(body.source).toBe('drive');
    expect(body.fids).toHaveLength(2);
    expect(body.fids).toEqual(expect.arrayContaining(['f1', 'f2']));
    expect(body.dest).toBe('');
    expect(body.mode).toBe('a');

    // 成功：Dialog 关闭 + 勾选清空（防重复提交的业务面）
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.getByText('已选 0 项')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '下载选中' })).toBeDisabled();
  });

  it('radio 组选 B（边下边播）：body.mode 透传 "b"；dest 填子目录透传', async () => {
    await prepareChecked();
    fireEvent.change(screen.getByLabelText('目标子目录'), {
      target: { value: '电影/2024' },
    });
    fireEvent.click(screen.getByRole('radio', { name: 'B（边下边播）' }));
    fireEvent.click(screen.getByRole('button', { name: '开始下载' }));
    await flush();

    const body = taskCall();
    expect(body.mode).toBe('b');
    expect(body.dest).toBe('电影/2024');
  });

  it('mode 选择为 fieldset/legend radio 组（a11y：可按组名寻得）', async () => {
    await prepareChecked();

    const group = screen.getByRole('group', { name: '下载模式' });
    expect(group).toBeInTheDocument();
    const radioA = screen.getByRole('radio', { name: 'A（标准下载）' }) as HTMLInputElement;
    const radioB = screen.getByRole('radio', { name: 'B（边下边播）' }) as HTMLInputElement;
    expect(radioA.checked).toBe(true); // 默认 a
    expect(radioB.checked).toBe(false);
  });

  it('skipped 条目逐条 toast（创建链 dest_busy 进 skipped 同样用户可见，tasks.go:307）', async () => {
    taskResponse = {
      task_ids: ['t-1'],
      skipped: [{ fid: 'f1', name: '报告.pdf', reason: '同一文件正在下载中' }],
    };
    await prepareChecked();
    fireEvent.click(screen.getByRole('button', { name: '开始下载' }));
    await flush();

    expect(toastShow).toHaveBeenCalledWith('已跳过：报告.pdf（同一文件正在下载中）', 'info');
  });
});

describe('网盘页：提交防重与 409 dest_busy（场景 4）', () => {
  it('提交中禁止重复提交：按钮 disabled + aria-busy，响应前不可再点', async () => {
    await prepareChecked();
    let resolvePost!: (value: unknown) => void;
    vi.mocked(api.post).mockImplementation(
      () =>
        new Promise((resolve) => {
          resolvePost = resolve;
        }),
    );
    // 点击前抓引用：Button loading 态 children 换 Spinner（accessible name
    // 暂失，Task 2 ui 既有行为），以元素引用断言提交态而非按名重查
    const submitBtn = screen.getByRole('button', { name: '开始下载' });
    fireEvent.click(submitBtn);

    expect(submitBtn).toBeDisabled();
    expect(submitBtn).toHaveAttribute('aria-busy', 'true');

    resolvePost({ task_ids: ['t-1'], skipped: [] });
    await flush();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('mock 409 dest_busy：Toast 文案取后端 message，Dialog 保留可重试', async () => {
    taskResponse = new ApiError('dest_busy', '同一文件正在下载中', 409);
    await prepareChecked();
    fireEvent.click(screen.getByRole('button', { name: '开始下载' }));
    await flush();

    expect(toastShow).toHaveBeenCalledWith('同一文件正在下载中', 'error');
    // Dialog 保留（用户可改 dest/取消重试），且退出提交态可再点
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '开始下载' })).toBeEnabled();
  });
});
