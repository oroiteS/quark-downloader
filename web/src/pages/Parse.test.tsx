// 解析页行为钉死（简报 Step 1 五场景 + 补充边界）：
// 1. 粘贴含链接文本 → POST /api/share/parse {text} → 列表渲染 file_name/size（1024 基格式化）
// 2. 目录行点击 → 以其 fid 调 share/files 并渲染子列表 + 面包屑；面包屑返回根
// 3. 勾选目录+文件 → 下载 Dialog → POST /api/tasks {source:'share', fids, sid, dest} → 跳 /tasks；skipped 逐条 toast
// 4. kouling:true 响应 → 显示 hint 不报错（无 role=alert、无列表）
// 5. 生成直链 → POST /api/links（选中 fids+sid）→ 导出 Dialog 五格式 tab 切换各调 export 且文本上屏
// 补充钉住：空文本前端拦截、parse 400 报错、sid 失效 404 报错、全选/部分选 indeterminate、
// dest 非空透传、下载失败不跳转、草稿写入/回填 session.pendingParseText（Focus #3）、空目录文案。
// 手法（与 Login.test 同型）：api 以 vi.mock 模块级替换；export 纯文本通道 stub 全局
// fetch；MemoryRouter + location 探针断言跳转；toast 以 spyOn 观测逐条反馈。
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
const FILE_A = { fid: 'f1', file_name: '报告.pdf', size: 1536, dir: false, share_fid_token: 't1' };
const DIR_B = { fid: 'd1', file_name: '剧集', size: 0, dir: true, share_fid_token: 't2' };
const FILE_C = { fid: 'f2', file_name: 'README.md', size: 4096, dir: false, share_fid_token: 't3' };
const SUB_FILE = { fid: 'f3', file_name: '第01集.mkv', size: 2 * 1024 ** 3, dir: false, share_fid_token: 't4' };
// 子目录内与根同名文件（fix round 1 W2：同名跨目录 rel_path 各归层级不冲突）
const SUB_SAME_NAME = { fid: 'f4', file_name: 'README.md', size: 4096, dir: false, share_fid_token: 't5' };

const PARSE_OK = { sid: 'sid-1', pwd_id: 'pwd-1', passcode: '', files: [FILE_A, DIR_B, FILE_C] };
const LINKS_OK = {
  items: [{ file_name: '报告.pdf', size: 1536, url: 'https://dl.example/u1', score: 10 }],
  expires_hint: '直链有时效，请尽快使用',
};

const SHARE_TEXT = '链接：https://pan.quark.cn/s/abcd 提取码：x9kz';

/** 可配置的 POST /api/share/parse 响应（kouling 用例覆写）。 */
let parseResponse: unknown;
/** 可配置的 POST /api/tasks 响应（skipped 逐条反馈断言用）。 */
let taskResponse: unknown;
/** 可配置的 POST /api/links 响应（W1/W2 导出用例覆写）。 */
let linksResponse: unknown;
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
    if (path === '/api/links') {
      const res = linksResponse;
      if (res instanceof ApiError) throw res;
      return res as never;
    }
    throw new Error(`unexpected POST ${path}`);
  });
}

/** export 纯文本通道（T3 账本：不走 apiFetch）——stub 全局 fetch，按 body.format 回显。 */
function stubFetchExport(): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (_url: string | URL, init?: RequestInit) => ({
      ok: true,
      status: 200,
      text: async () =>
        `TEXT-${(JSON.parse(String(init?.body)) as { format: string }).format}`,
    })),
  );
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
  linksResponse = LINKS_OK;
  dirTable = { '': [FILE_A, DIR_B, FILE_C], d1: [SUB_FILE, SUB_SAME_NAME] };
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
  stubFetchExport();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
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
    expect(screen.getByText('1.5 KB')).toBeInTheDocument();
    expect(screen.getByText('4 KB')).toBeInTheDocument();
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
    expect(screen.getByText('2 GB')).toBeInTheDocument();

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

  it('未勾选时下载/直链按钮禁用', async () => {
    renderParse();
    await parseShareText();

    expect(screen.getByRole('button', { name: '下载选中' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '生成直链' })).toBeDisabled();
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

  it('Dialog 取消：关闭对话框不请求', async () => {
    await prepareChecked();
    const before = vi.mocked(api.post).mock.calls.length;
    fireEvent.click(screen.getByRole('button', { name: '取消' }));

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(vi.mocked(api.post).mock.calls.length).toBe(before);
  });
});

describe('解析页：生成直链与五格式导出', () => {
  async function openExport(): Promise<void> {
    renderParse();
    await parseShareText();
    fireEvent.click(screen.getByRole('checkbox', { name: '选择 报告.pdf' }));
    fireEvent.click(screen.getByRole('button', { name: '生成直链' }));
    await screen.findByRole('dialog');
  }

  it('POST links {source, fids, sid} → 导出 Dialog 打开，时效提示上屏', async () => {
    await openExport();

    expect(vi.mocked(api.post)).toHaveBeenCalledWith('/api/links', {
      source: 'share',
      fids: ['f1'],
      sid: 'sid-1',
    });
    expect(screen.getByRole('dialog')).toHaveTextContent(
      '直链有时效，请尽快使用',
    );
  });

  it('五格式 tab 切换各调一次 export（rel_path=file_name），文本上屏', async () => {
    await openExport();
    await vi.waitFor(() =>
      expect(screen.getByLabelText('导出内容')).toHaveValue('TEXT-plain'),
    );

    const tabs = [
      ['纯文本', 'plain'],
      ['带文件名', 'named'],
      ['aria2', 'aria2'],
      ['curl', 'curl'],
      ['Motrix', 'motrix'],
    ] as const;
    const tabEls = tabs.map(([label]) => screen.getByRole('tab', { name: label }));
    expect(tabEls).toHaveLength(5);
    expect(screen.getByRole('tablist', { name: '导出格式' })).toBeInTheDocument();
    expect(tabEls[0]).toHaveAttribute('aria-selected', 'true');

    const fetchMock = vi.mocked(globalThis.fetch);
    const firstBody = JSON.parse(
      String(fetchMock.mock.calls[0][1]?.body),
    ) as { items: unknown[]; format: string };
    expect(firstBody).toEqual({
      items: [{ rel_path: '报告.pdf', url: 'https://dl.example/u1' }],
      format: 'plain',
    });

    // 逐个切换：aria-selected 迁移 + 各自调 export 且文本上屏
    for (let i = 1; i < tabs.length; i += 1) {
      fireEvent.click(tabEls[i]);
      await vi.waitFor(() =>
        expect(screen.getByLabelText('导出内容')).toHaveValue(
          `TEXT-${tabs[i][1]}`,
        ),
      );
      expect(tabEls[i]).toHaveAttribute('aria-selected', 'true');
      expect(tabEls[0]).toHaveAttribute('aria-selected', 'false');
    }

    const formats = (
      fetchMock.mock.calls.map(
        ([, init]) => JSON.parse(String(init?.body)) as { format: string },
      )
    ).map((b) => b.format);
    expect(formats).toEqual(['plain', 'named', 'aria2', 'curl', 'motrix']);
  });

  it('tab 支持方向键切换（ARIA tabs 键盘模式）', async () => {
    await openExport();
    await vi.waitFor(() =>
      expect(screen.getByLabelText('导出内容')).toHaveValue('TEXT-plain'),
    );

    fireEvent.keyDown(screen.getByRole('tab', { name: '纯文本' }), { key: 'ArrowRight' });
    expect(screen.getByRole('tab', { name: '带文件名' })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    // 在 aria2 tab 上按 ArrowLeft：从 aria2 退回带文件名（仍选中不变）
    fireEvent.keyDown(screen.getByRole('tab', { name: 'aria2' }), { key: 'ArrowLeft' });
    expect(screen.getByRole('tab', { name: '带文件名' })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    await flush();
  });

  it('export 失败：Dialog 内 role=alert 显示原因', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => ({
        ok: false,
        status: 400,
        json: async () => ({ error: { code: 'invalid_links_request', message: '未知导出格式: xxx' } }),
      })),
    );
    await openExport();

    expect(await screen.findByRole('alert')).toHaveTextContent('未知导出格式');
  });

  it('复制按钮：clipboard 写入文本并 toast 反馈', async () => {
    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    });
    await openExport();
    await vi.waitFor(() =>
      expect(screen.getByLabelText('导出内容')).toHaveValue('TEXT-plain'),
    );

    fireEvent.click(screen.getByRole('button', { name: '复制' }));
    await vi.waitFor(() => expect(writeText).toHaveBeenCalledWith('TEXT-plain'));
    expect(toastShow).toHaveBeenCalledWith('已复制到剪贴板', 'success');
  });

  // ---- fix round 1 W1：目录项不参与直链 ----
  it('勾选目录+文件生成直链：links 请求仅含文件 fid，toast「已忽略 1 个目录」', async () => {
    renderParse();
    await parseShareText();
    fireEvent.click(screen.getByRole('checkbox', { name: '选择 报告.pdf' }));
    fireEvent.click(
      screen.getByRole('checkbox', { name: '选择 剧集（含子目录）' }),
    );
    fireEvent.click(screen.getByRole('button', { name: '生成直链' }));
    await screen.findByRole('dialog');

    expect(vi.mocked(api.post)).toHaveBeenCalledWith('/api/links', {
      source: 'share',
      fids: ['f1'],
      sid: 'sid-1',
    });
    expect(toastShow).toHaveBeenCalledWith('已忽略 1 个目录', 'info');
  });

  it('仅勾选目录：生成直链禁用，不请求 links', async () => {
    renderParse();
    await parseShareText();
    fireEvent.click(
      screen.getByRole('checkbox', { name: '选择 剧集（含子目录）' }),
    );

    const btn = screen.getByRole('button', { name: '生成直链' });
    expect(btn).toBeDisabled();
    fireEvent.click(btn);
    await flush();
    expect(
      vi.mocked(api.post).mock.calls.some(([p]) => p === '/api/links'),
    ).toBe(false);
  });

  // ---- fix round 1 W2：勾选路径上下文 → rel_path 含层级 ----
  it('子目录内勾选生成直链：导出 rel_path 含面包屑层级', async () => {
    linksResponse = {
      items: [
        {
          file_name: '第01集.mkv',
          size: 2 * 1024 ** 3,
          url: 'https://dl.example/u3',
          score: 10,
        },
      ],
      expires_hint: '直链有时效，请尽快使用',
    };
    renderParse();
    await parseShareText();
    fireEvent.click(screen.getByRole('button', { name: '剧集' }));
    fireEvent.click(
      await screen.findByRole('checkbox', { name: '选择 第01集.mkv' }),
    );
    fireEvent.click(screen.getByRole('button', { name: '生成直链' }));
    await screen.findByRole('dialog');
    await vi.waitFor(() =>
      expect(screen.getByLabelText('导出内容')).toHaveValue('TEXT-plain'),
    );

    const fetchMock = vi.mocked(globalThis.fetch);
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body)) as {
      items: Array<{ rel_path: string; url: string }>;
    };
    expect(body.items).toEqual([
      { rel_path: '剧集/第01集.mkv', url: 'https://dl.example/u3' },
    ]);
  });

  it('跨目录同名文件勾选导出：rel_path 按勾选序各归层级，重名不冲突', async () => {
    linksResponse = {
      items: [
        { file_name: 'README.md', size: 4096, url: 'https://dl.example/u-root', score: 10 },
        { file_name: 'README.md', size: 4096, url: 'https://dl.example/u-sub', score: 10 },
      ],
      expires_hint: '直链有时效，请尽快使用',
    };
    renderParse();
    await parseShareText();
    // 根列表 README.md（f2）先勾，子目录同名 README.md（f4）后勾
    fireEvent.click(screen.getByRole('checkbox', { name: '选择 README.md' }));
    fireEvent.click(screen.getByRole('button', { name: '剧集' }));
    fireEvent.click(
      await screen.findByRole('checkbox', { name: '选择 README.md' }),
    );
    fireEvent.click(screen.getByRole('button', { name: '生成直链' }));
    await screen.findByRole('dialog');
    await vi.waitFor(() =>
      expect(screen.getByLabelText('导出内容')).toHaveValue('TEXT-plain'),
    );

    const fetchMock = vi.mocked(globalThis.fetch);
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body)) as {
      items: Array<{ rel_path: string; url: string }>;
    };
    expect(body.items).toEqual([
      { rel_path: 'README.md', url: 'https://dl.example/u-root' },
      { rel_path: '剧集/README.md', url: 'https://dl.example/u-sub' },
    ]);
  });
});
