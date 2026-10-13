// 解析页行为钉死（简报 Step 1 四场景 + 补充边界）：
// 1. 粘贴含链接文本 → POST /api/share/parse {text} → 列表渲染 file_name/size（GiB 两位小数统一格式，Ruling T7-W1）
// 2. 目录行点击 → 以其 fid 调 share/files 并渲染子列表 + 面包屑；面包屑返回根
// 3. 勾选目录+文件 → 下载 Dialog → POST /api/tasks {source:'share', fids, sid, dest} → 跳 /tasks；skipped 逐条 toast
// 4. kouling:true 响应 → 显示 hint 不报错（无 role=alert、无列表）
// 补充钉住：空文本前端拦截、parse 400 报错、sid 失效 404 报错、全选/部分选 indeterminate、
// dest 非空透传、下载失败不跳转、草稿写入/回填 session.pendingParseText（Focus #3）、空目录文案。
// 手法（与 Login.test 同型）：api 以 vi.mock 模块级替换；MemoryRouter + location
// 探针断言跳转；toast 以 spyOn 观测逐条反馈。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { Parse } from './Parse';
import { api } from '../api/client';
import { ApiError } from '../api/types';
import { toast } from '../components/ui';
import { useSession } from '../stores/session';

vi.mock('../api/client', () => ({
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn() },
}));

// ---- 夹具（与后端实测响应形状逐字一致）----
const FILE_A = { fid: 'f1', file_name: '报告.pdf', size: 2 * 1024 ** 3, dir: false, share_fid_token: 't1' };
const DIR_B = { fid: 'd1', file_name: '剧集', size: 0, dir: true, share_fid_token: 't2' };
const FILE_C = { fid: 'f2', file_name: 'README.md', size: 3 * 1024 ** 3, dir: false, share_fid_token: 't3' };
const SUB_FILE = { fid: 'f3', file_name: '第01集.mkv', size: 2 * 1024 ** 3, dir: false, share_fid_token: 't4' };

const PARSE_OK = { sid: 'sid-1', pwd_id: 'pwd-1', passcode: '', files: [FILE_A, DIR_B, FILE_C] };

const SHARE_TEXT = '链接：https://pan.quark.cn/s/abcd 提取码：x9kz';

/** 可配置的 POST /api/share/parse 响应（kouling 用例覆写）。 */
let parseResponse: unknown;
/** 可配置的 POST /api/tasks 响应（skipped 逐条反馈断言用）。 */
let taskResponse: unknown;
/** share/files 目录表：pdir_fid → files（'' 为根）。 */
let dirTable: Record<string, unknown[]>;
/** 记录 share/files 实际请求过的 pdir_fid 序列。 */
let filesLog: string[];

function mockApiDefaults(): void {
  vi.mocked(api.get).mockImplementation(async (path: string) => {
    if (path.startsWith('/api/share/files')) {
      const pdir = new URLSearchParams(path.split('?')[1]).get('pdir_fid') ?? '';
      filesLog.push(pdir);
      return { files: dirTable[pdir] ?? [] } as never;
    }
    throw new Error(`unexpected GET ${path}`);
  });
  vi.mocked(api.post).mockImplementation(async (path: string) => {
    if (path === '/api/share/parse') {
      const res = parseResponse;
      if (res instanceof ApiError) throw res;
      return res as never;
    }
    if (path === '/api/tasks') {
      const res = taskResponse;
      if (res instanceof ApiError) throw res;
      return res as never;
    }
    throw new Error(`unexpected POST ${path}`);
  });
}

let seen: string[] = [];
function LocationProbe() {
  const { pathname } = useLocation();
  if (seen[seen.length - 1] !== pathname) {
    seen.push(pathname);
  }
  return <span data-testid="loc">{pathname}</span>;
}

function renderParse(): void {
  seen = [];
  render(
    <MemoryRouter initialEntries={['/']}>
      <LocationProbe />
      <Parse />
    </MemoryRouter>,
  );
}

/** flush 异步状态更新（解析/建任务/导航后的微任务链）。 */
async function flush(): Promise<void> {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

async function parseShareText(text = SHARE_TEXT): Promise<void> {
  fireEvent.change(screen.getByLabelText('分享链接文本'), {
    target: { value: text },
  });
  fireEvent.click(screen.getByRole('button', { name: '解析' }));
  // 不等列表（kouling 场景无列表）：等异步链落定后由用例自行断言
  await flush();
}

function parseCalls(): Array<[string, unknown]> {
  return vi.mocked(api.post).mock.calls.filter(
    ([p]) => p === '/api/share/parse',
  ) as Array<[string, unknown]>;
}

function taskCall(): { source: string; fids: string[]; sid: string; dest: string } {
  const call = vi.mocked(api.post).mock.calls.find(([p]) => p === '/api/tasks');
  expect(call).toBeDefined();
  return call![1] as { source: string; fids: string[]; sid: string; dest: string };
}

let toastShow: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  parseResponse = PARSE_OK;
  taskResponse = {
    task_ids: ['t-1', 't-2'],
    skipped: [{ fid: 'f1', name: '报告.pdf', reason: '已存在' }],
  };
  dirTable = { '': [FILE_A, DIR_B, FILE_C], d1: [SUB_FILE] };
  filesLog = [];
  useSession.setState({
    loggedIn: true,
    nickname: '',
    avatar: '',
    capacity: null,
    pendingParseText: '',
  });
  toastShow = vi.spyOn(toast, 'show');
  mockApiDefaults();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  useSession.setState({ loggedIn: null, pendingParseText: '' });
});

describe('解析页：解析与列表渲染', () => {
  it('粘贴含链接文本点解析：POST {text}，列表渲染 file_name 与 1024 基格式化大小', async () => {
    renderParse();
    await parseShareText();

    expect(parseCalls()).toHaveLength(1);
    expect(parseCalls()[0][1]).toEqual({ text: SHARE_TEXT });
    expect(screen.getByRole('table', { name: '文件列表' })).toBeInTheDocument();
    expect(screen.getByText('剧集')).toBeInTheDocument();
    expect(screen.getByText('README.md')).toBeInTheDocument();
    expect(screen.getByText('2.00 GiB')).toBeInTheDocument();
    expect(screen.getByText('3.00 GiB')).toBeInTheDocument();
  });

  it('输入即写入 session.pendingParseText；重挂载草稿回填（Focus #3）', async () => {
    renderParse();
    fireEvent.change(screen.getByLabelText('分享链接文本'), {
      target: { value: '草稿文本' },
    });
    expect(useSession.getState().pendingParseText).toBe('草稿文本');

    cleanup();
    renderParse();
    expect(
      (screen.getByLabelText('分享链接文本') as HTMLTextAreaElement).value,
    ).toBe('草稿文本');
  });

  it('空文本点解析：前端拦截不发请求，role=alert 提示', () => {
    renderParse();
    fireEvent.click(screen.getByRole('button', { name: '解析' }));

    expect(parseCalls()).toHaveLength(0);
    expect(screen.getByRole('alert')).toHaveTextContent('请粘贴分享链接');
  });

  it('parse 400（未识别到分享链接）：role=alert 显示后端原因，不渲染列表', async () => {
    parseResponse = new ApiError('invalid_share_text', '未识别到分享链接', 400);
    renderParse();
    await parseShareText();

    expect(screen.getByRole('alert')).toHaveTextContent('未识别到分享链接');
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
  });

  it('kouling:true：显示 hint 不报错（无 role=alert、无列表）', async () => {
    parseResponse = { kouling: true, hint: '口令请改用分享链接' };
    renderParse();
    await parseShareText('随便一段口令文本');

    expect(screen.getByText('口令请改用分享链接')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
  });
});

describe('解析页：目录导航（G10）', () => {
  it('目录行点击：以其 fid 调 share/files，渲染子列表与面包屑；面包屑返回根重取根列表', async () => {
    renderParse();
    await parseShareText();

    fireEvent.click(screen.getByRole('button', { name: '剧集' }));
    await screen.findByText('第01集.mkv');

    // 根列表直接来自 parse 响应（不调 share/files）；进入子目录是首次调用
    expect(filesLog).toEqual(['d1']);
    expect(vi.mocked(api.get).mock.calls[0][0]).toBe(
      '/api/share/files?sid=sid-1&pdir_fid=d1',
    );
    expect(screen.getByText('2.00 GiB')).toBeInTheDocument();

    // 面包屑：根 + 当前目录；当前目录 aria-current 标记
    const crumbs = screen.getByRole('navigation', { name: '目录路径' });
    expect(crumbs).toHaveTextContent('分享根目录');
    expect(crumbs).toHaveTextContent('剧集');
    expect(screen.getByText('剧集')).toHaveAttribute('aria-current', 'location');

    // 点根面包屑 → 回根列表（pdir_fid='' 重取）
    fireEvent.click(
      screen.getByRole('button', { name: '分享根目录' }),
    );
    await screen.findByText('报告.pdf');
    expect(filesLog).toEqual(['d1', '']);
    expect(screen.queryByText('第01集.mkv')).not.toBeInTheDocument();
  });

  it('sid 失效（404）：role=alert 提示重新解析，原列表保留不崩溃', async () => {
    renderParse();
    await parseShareText();
    vi.mocked(api.get).mockRejectedValue(
      new ApiError('no_share_session', '分享会话不存在或已失效, 请重新解析', 404),
    );
    fireEvent.click(screen.getByRole('button', { name: '剧集' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      '分享会话不存在或已失效, 请重新解析',
    );
    expect(screen.getByText('报告.pdf')).toBeInTheDocument();
  });

  it('目录返回空列表：显示空目录文案', async () => {
    parseResponse = { ...PARSE_OK, files: [] };
    renderParse();
    await parseShareText();

    expect(screen.getByText('此目录为空')).toBeInTheDocument();
  });

  it('T16 防御：parse 响应 files:null → 渲染空目录文案不崩（files.map 不遇 null）', async () => {
    parseResponse = { ...PARSE_OK, files: null };
    renderParse();
    await parseShareText();

    expect(screen.getByRole('table', { name: '文件列表' })).toBeInTheDocument();
    expect(screen.getByText('此目录为空')).toBeInTheDocument();
  });

  it('T16 防御：share/files 响应 files:null（进入子目录）→ 空目录文案不崩', async () => {
    renderParse();
    await parseShareText();
    vi.mocked(api.get).mockImplementation(async () => ({ files: null }) as never);
    fireEvent.click(screen.getByRole('button', { name: '剧集' }));
    await flush();

    expect(screen.getByText('此目录为空')).toBeInTheDocument();
  });
});

describe('解析页：FileList 勾选（通用列表契约）', () => {
  it('复选框 aria-label 含文件名，目录项标注「含子目录」；全选切换全部行；部分勾选时全选框 indeterminate', async () => {
    renderParse();
    await parseShareText();

    const checkA = screen.getByRole('checkbox', { name: '选择 报告.pdf' });
    const checkB = screen.getByRole('checkbox', {
      name: '选择 剧集（含子目录）',
    });
    const checkC = screen.getByRole('checkbox', { name: '选择 README.md' });
    expect(screen.getByText('含子目录')).toBeInTheDocument();

    fireEvent.click(checkA);
    expect(checkA).toBeChecked();

    const all = screen.getByRole('checkbox', { name: '全选' }) as HTMLInputElement;
    expect(all.checked).toBe(false);
    expect(all.indeterminate).toBe(true); // 部分 → mixed

    fireEvent.click(all);
    expect(checkA).toBeChecked();
    expect(checkB).toBeChecked();
    expect(checkC).toBeChecked();
    expect(all.checked).toBe(true);
    expect(all.indeterminate).toBe(false);

    fireEvent.click(all);
    expect(checkA).not.toBeChecked();
    expect(checkB).not.toBeChecked();
  });

  it('目录导航后勾选保留（fid 全局集合），跨目录累计选中', async () => {
    renderParse();
    await parseShareText();
    fireEvent.click(screen.getByRole('checkbox', { name: '选择 报告.pdf' }));

    fireEvent.click(screen.getByRole('button', { name: '剧集' }));
    const sub = await screen.findByRole('checkbox', { name: '选择 第01集.mkv' });
    fireEvent.click(sub);

    // 返回根：先前的勾选仍在
    fireEvent.click(screen.getByRole('button', { name: '分享根目录' }));
    await screen.findByText('报告.pdf');
    expect(
      screen.getByRole('checkbox', { name: '选择 报告.pdf' }),
    ).toBeChecked();
  });
});

describe('解析页：勾选下载', () => {
  async function prepareChecked(): Promise<void> {
    renderParse();
    await parseShareText();
    fireEvent.click(screen.getByRole('checkbox', { name: '选择 报告.pdf' }));
    fireEvent.click(
      screen.getByRole('checkbox', { name: '选择 剧集（含子目录）' }),
    );
    fireEvent.click(screen.getByRole('button', { name: '下载选中' }));
    await screen.findByRole('dialog');
  }

  it('未勾选时下载按钮禁用', async () => {
    renderParse();
    await parseShareText();

    expect(screen.getByRole('button', { name: '下载选中' })).toBeDisabled();
  });

  it('默认 dest 空：POST tasks {source, fids, sid, dest:""} → 跳 /tasks，skipped 逐条 toast', async () => {
    await prepareChecked();
    fireEvent.click(screen.getByRole('button', { name: '开始下载' }));
    await flush();

    const body = taskCall();
    expect(body.source).toBe('share');
    expect(body.sid).toBe('sid-1');
    expect(body.dest).toBe('');
    expect(body.fids).toHaveLength(2);
    expect(body.fids).toEqual(expect.arrayContaining(['f1', 'd1']));

    expect(toastShow).toHaveBeenCalledWith('已跳过：报告.pdf（已存在）', 'info');
    expect(screen.getByTestId('loc')).toHaveTextContent('/tasks');
    expect(seen).toEqual(['/', '/tasks']);
  });

  it('dest 填子目录：请求透传 dest', async () => {
    await prepareChecked();
    fireEvent.change(screen.getByLabelText('目标子目录'), {
      target: { value: '电影/2024' },
    });
    fireEvent.click(screen.getByRole('button', { name: '开始下载' }));
    await flush();

    expect(taskCall().dest).toBe('电影/2024');
  });

  it('下载失败：Dialog 内 role=alert 显示原因，不跳转', async () => {
    taskResponse = new ApiError('submit_failed', '任务提交失败', 502);
    await prepareChecked();
    fireEvent.click(screen.getByRole('button', { name: '开始下载' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('任务提交失败');
    expect(screen.getByTestId('loc')).toHaveTextContent('/');
    expect(seen).toEqual(['/']);
  });

  // T10 真机复验二轮：成功路径曾误报兜底文案（非 ApiError 的 fetch 层
  // reject），console 必须留原始错误现场供真机复现定位。
  it('非 ApiError 失败：console.error 留现场 + 兜底文案，不跳转', async () => {
    const errSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    await prepareChecked();
    vi.mocked(api.post).mockRejectedValue(new TypeError('Failed to fetch'));
    fireEvent.click(screen.getByRole('button', { name: '开始下载' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      '任务提交失败，请稍后重试',
    );
    expect(errSpy).toHaveBeenCalledWith('任务提交失败', expect.any(TypeError));
    expect(seen).toEqual(['/']);
  });

  // 防双击（对齐 Drive 页防重语义）：提交中按钮 disabled + aria-busy，
  // 响应前不可再点（组件层 Button disabled||loading，页面级钉死用法）。
  it('提交中禁止重复提交：按钮 disabled + aria-busy，响应前不可再点', async () => {
    await prepareChecked();
    let resolvePost!: (value: unknown) => void;
    vi.mocked(api.post).mockImplementation(
      () =>
        new Promise((resolve) => {
          resolvePost = resolve;
        }),
    );
    // 点击前抓引用：Button loading 态 children 保留（Spinner aria-hidden
    // 不占可及名），提交态断言沿用该元素引用
    const submitBtn = screen.getByRole('button', { name: '开始下载' });
    fireEvent.click(submitBtn);

    expect(submitBtn).toBeDisabled();
    expect(submitBtn).toHaveAttribute('aria-busy', 'true');

    resolvePost({ task_ids: ['t-1'], skipped: [] });
    await flush();
    expect(screen.getByTestId('loc')).toHaveTextContent('/tasks');
  });

  it('Dialog 取消：关闭对话框不请求', async () => {
    await prepareChecked();
    const before = vi.mocked(api.post).mock.calls.length;
    fireEvent.click(screen.getByRole('button', { name: '取消' }));

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(vi.mocked(api.post).mock.calls.length).toBe(before);
  });
});
