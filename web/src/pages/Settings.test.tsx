// 设置页行为钉死（简报 Step 1 四场景 + 补充边界）：
// 1. 加载回显：GET /api/settings 四字段落 Input/下拉/radio
// 2. 仅改主题为深色 → 保存：PUT body 四字段齐发（themeLock="dark"，
//    destDir/threads/max 原值——全量替换语义，Review Focus #2）；成功 Toast
// 3. threads 缺省（非档位值）→ 并发档位下拉渲染 32（G5）
// 4. 退出登录：Dialog 确认 → POST /api/logout → session 清 → 跳 /login
// 补充钉住：主题切换即时 applyTheme（D7：data-theme 即变不必等保存，含跟随
// 分支的 matchMedia 驱动）；空 destDir 保存 → 400 invalid_settings 回显后端
// message（role=alert）；保存中防重复提交；logout 失败 toast error 停留
// Dialog；GET 失败 role=alert。
// 手法（与 Drive.test 同型）：api 以 vi.mock 模块级替换；toast 以 spyOn 观测；
// jsdom 未实现 matchMedia——按 theme.test 同款 fake 注入（跟随分支需要）。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { Settings } from './Settings';
import { api } from '../api/client';
import { ApiError } from '../api/types';
import { toast } from '../components/ui';
import { useSession } from '../stores/session';

vi.mock('../api/client', () => ({
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn() },
}));

// ---- 夹具（与后端实测响应形状逐字一致：webapi/settings.go json tag）----
const SETTINGS = {
  destDir: '/tmp/dl',
  threads: 64,
  maxConcurrentTasks: 3,
  themeLock: 'light',
};

/** jsdom 无 matchMedia（theme.test 同款）：跟随分支 applyTheme("") 需要。 */
function stubMatchMedia(matches = false): void {
  vi.spyOn(window, 'matchMedia').mockReturnValue({
    matches,
    addEventListener: () => {},
    removeEventListener: () => {},
  } as unknown as MediaQueryList);
}

function mockGetSettings(value: unknown = SETTINGS): void {
  vi.mocked(api.get).mockImplementation(async (path: string) => {
    if (path === '/api/settings') return value as never;
    throw new Error(`unexpected GET ${path}`);
  });
}

/** flush 异步状态更新（挂载拉取/保存/登出的微任务链）。 */
async function flush(): Promise<void> {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

/** /settings 挂真实路由，/login 为占位文本——logout 跳转断言走真导航。 */
function renderSettings(): void {
  render(
    <MemoryRouter initialEntries={['/settings']}>
      <Routes>
        <Route path="/settings" element={<Settings />} />
        <Route path="/login" element={<p>登录页占位</p>} />
      </Routes>
    </MemoryRouter>,
  );
}

/** 渲染并等加载完成（加载态无表单 Input，findBy 即等待拉取结束）。 */
async function renderLoaded(): Promise<void> {
  renderSettings();
  await screen.findByLabelText('下载目录');
}

let toastShow: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  stubMatchMedia();
  mockGetSettings();
  vi.mocked(api.put).mockResolvedValue(SETTINGS as never);
  vi.mocked(api.post).mockResolvedValue({ logged_in: false } as never);
  toastShow = vi.spyOn(toast, 'show');
  useSession.setState({
    loggedIn: true,
    nickname: '阿珍',
    avatar: '',
    capacity: null,
  });
  document.documentElement.removeAttribute('data-theme');
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  document.documentElement.removeAttribute('data-theme');
});

describe('设置页：加载回显（场景 1）', () => {
  it('挂载即 GET /api/settings，四字段落 Input/下拉/radio', async () => {
    await renderLoaded();

    expect(vi.mocked(api.get)).toHaveBeenCalledWith('/api/settings');
    expect(screen.getByLabelText('下载目录')).toHaveValue('/tmp/dl');
    expect(screen.getByLabelText('并发档位')).toHaveValue('64');
    expect(screen.getByLabelText('同时任务数')).toHaveValue('3');
    expect(screen.getByRole('radio', { name: '浅色' })).toBeChecked();
    expect(screen.getByRole('radio', { name: '跟随系统' })).not.toBeChecked();
    expect(screen.getByRole('radio', { name: '深色' })).not.toBeChecked();
  });

  it('加载中：role=status 提示且不渲染表单', () => {
    vi.mocked(api.get).mockImplementation(() => new Promise(() => {}));
    renderSettings();

    expect(screen.getByRole('status')).toBeInTheDocument();
    expect(screen.queryByLabelText('下载目录')).not.toBeInTheDocument();
  });

  it('GET 失败：role=alert 显示后端文案', async () => {
    vi.mocked(api.get).mockRejectedValue(
      new ApiError('internal', '设置读取失败: boom', 500),
    );
    renderSettings();

    expect(await screen.findByRole('alert')).toHaveTextContent(
      '设置读取失败: boom',
    );
  });
});

describe('设置页：全量保存语义（场景 2，Review Focus #2）', () => {
  it('仅改主题为深色 → 保存：PUT body 四字段齐发，destDir/threads/max 原值；成功 Toast', async () => {
    await renderLoaded();
    fireEvent.click(screen.getByRole('radio', { name: '深色' }));
    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    await flush();

    expect(vi.mocked(api.put)).toHaveBeenCalledTimes(1);
    expect(vi.mocked(api.put)).toHaveBeenCalledWith('/api/settings', {
      destDir: '/tmp/dl',
      threads: 64,
      maxConcurrentTasks: 3,
      themeLock: 'dark',
    });
    expect(toastShow).toHaveBeenCalledWith('设置已保存', 'success');
  });

  it('空 destDir 保存 → 400 invalid_settings：role=alert 回显后端 message，无成功 Toast', async () => {
    await renderLoaded();
    vi.mocked(api.put).mockRejectedValue(
      new ApiError('invalid_settings', 'destDir 不可为空', 400),
    );
    fireEvent.change(screen.getByLabelText('下载目录'), {
      target: { value: '' },
    });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'destDir 不可为空',
    );
    expect(toastShow).not.toHaveBeenCalledWith('设置已保存', 'success');
  });

  it('保存中防重复提交：按钮 disabled + aria-busy，响应前不可再点', async () => {
    await renderLoaded();
    let resolvePut!: (value: unknown) => void;
    vi.mocked(api.put).mockImplementation(
      () =>
        new Promise((resolve) => {
          resolvePut = resolve;
        }),
    );
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    const saveBtn = screen.getByRole('button', { name: '保存' });
    expect(saveBtn).toBeDisabled();
    expect(saveBtn).toHaveAttribute('aria-busy', 'true');

    resolvePut(SETTINGS);
    await flush();
    expect(screen.getByRole('button', { name: '保存' })).toBeEnabled();
  });
});

describe('设置页：主题即时生效（D7）', () => {
  it('radio 切深色 → data-theme 即变 dark 不等保存；切跟随系统 → 按 matchMedia 现值', async () => {
    await renderLoaded();

    fireEvent.click(screen.getByRole('radio', { name: '深色' }));
    expect(document.documentElement).toHaveAttribute('data-theme', 'dark');

    // 跟随分支：stub matches=false → 现值为 light
    fireEvent.click(screen.getByRole('radio', { name: '跟随系统' }));
    expect(document.documentElement).toHaveAttribute('data-theme', 'light');
  });
});

describe('设置页：threads 缺省渲染 32（场景 3，G5）', () => {
  it('GET 返回非档位值（0）→ 下拉渲染 32，保存按展示值 32 上送', async () => {
    mockGetSettings({ ...SETTINGS, threads: 0 });
    await renderLoaded();

    expect(screen.getByLabelText('并发档位')).toHaveValue('32');

    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    await flush();
    expect(vi.mocked(api.put)).toHaveBeenCalledWith(
      '/api/settings',
      expect.objectContaining({ threads: 32 }),
    );
  });
});

describe('设置页：退出登录（场景 4）', () => {
  it('Dialog 确认 → POST /api/logout → session 清（loggedIn=false、账户字段清）→ 跳 /login', async () => {
    await renderLoaded();

    fireEvent.click(screen.getByRole('button', { name: '退出登录' }));
    const dialog = await screen.findByRole('dialog', { name: '退出登录' });
    expect(dialog).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: '确认退出' }));
    await flush();

    expect(vi.mocked(api.post)).toHaveBeenCalledWith('/api/logout');
    const session = useSession.getState();
    expect(session.loggedIn).toBe(false);
    expect(session.nickname).toBe('');
    expect(await screen.findByText('登录页占位')).toBeInTheDocument();
  });

  it('logout 失败：toast error 后端文案，Dialog 保留可重试且不跳转', async () => {
    await renderLoaded();
    fireEvent.click(screen.getByRole('button', { name: '退出登录' }));
    await screen.findByRole('dialog');
    vi.mocked(api.post).mockRejectedValue(
      new ApiError('internal', '服务不可用', 500),
    );

    fireEvent.click(screen.getByRole('button', { name: '确认退出' }));
    await flush();

    expect(toastShow).toHaveBeenCalledWith('服务不可用', 'error');
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(screen.queryByText('登录页占位')).not.toBeInTheDocument();
  });
});
