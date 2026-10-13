// ExportDialog 行为钉死（简报 Step 1：五格式 tab 切换各调 export 且文本上屏）：
// - open 即按当前格式（默认 plain）调 exportLinksText，items（rel_path+url）原样透传
//   （fix round 1 W2：rel_path 的层级组装责任在父层 Parse，组件不再以 file_name 二次组装）
// - 五 tab aria 语义（tablist/tab/aria-selected/tabpanel）+ 方向键/Home/End 键盘模式
// - 切换格式各自重新调 export，文本上屏；快速切换旧响应不覆盖新格式（代际防护）
// - 复制按钮 → clipboard.writeText(当前文本) + toast 反馈；clipboard 不可用 → 失败 toast
// - export 失败 → role=alert；加载中复制禁用；expiresHint 时效提示上屏
// export 通道为纯文本（T3 账本：apiFetch 2xx 硬 res.json() 会崩）——stub 全局 fetch。
// 样式断言直接读源 CSS 文本（jsdom 不做布局、断言不了计算样式）。运行时由
// vitest（Node）解析；TS 侧项目刻意无 @types/node（离线不新增依赖），局部豁免。
// @ts-expect-error 项目无 Node 类型定义（见上），运行时 vitest 提供实现
import { readFileSync } from 'node:fs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ExportDialog } from './ExportDialog';
import { toast } from './ui';

const ITEMS = [
  { rel_path: 'a.mp4', url: 'https://dl.example/1' },
  { rel_path: 'sub/b.mkv', url: 'https://dl.example/2' },
];
const EXPIRES_HINT = '直链有时效，请尽快使用';

const FORMATS = ['plain', 'named', 'aria2', 'curl', 'motrix'] as const;

/** 按请求 body.format 回显文本的 fetch stub；返回 mock 供逐用例覆写。 */
function stubFetchExport(): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(async (_url: string | URL, init?: RequestInit) => ({
    ok: true,
    status: 200,
    text: async () =>
      `TEXT-${(JSON.parse(String(init?.body)) as { format: string }).format}`,
  }));
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

let toastShow: ReturnType<typeof vi.spyOn>;

function renderDialog(
  overrides: {
    open?: boolean;
    onClose?: () => void;
    items?: Array<{ rel_path: string; url: string }>;
    expiresHint?: string;
  } = {},
): void {
  const {
    open = true,
    onClose = () => {},
    items = ITEMS,
    expiresHint = EXPIRES_HINT,
  } = overrides;
  render(
    <ExportDialog
      open={open}
      onClose={onClose}
      items={items}
      expiresHint={expiresHint}
    />,
  );
}

function exportBodies(fetchMock: ReturnType<typeof vi.fn>): Array<{
  items: Array<{ rel_path: string; url: string }>;
  format: string;
}> {
  return fetchMock.mock.calls.map(
    ([, init]) => JSON.parse(String(init?.body)) as never,
  );
}

beforeEach(() => {
  toastShow = vi.spyOn(toast, 'show');
  stubFetchExport();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('ExportDialog', () => {
  it('open=false 不渲染', () => {
    renderDialog({ open: false });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('打开默认 plain：五 tab 齐备、aria-selected 正确、文本上屏、items 原样透传', async () => {
    renderDialog();

    const tablist = screen.getByRole('tablist', { name: '导出格式' });
    expect(tablist).toBeInTheDocument();
    const tabs = FORMATS.map((f) => screen.getByRole('tab', { name: TAB_LABEL[f] }));
    expect(tabs).toHaveLength(5);
    expect(tabs[0]).toHaveAttribute('aria-selected', 'true');
    for (const tab of tabs.slice(1)) {
      expect(tab).toHaveAttribute('aria-selected', 'false');
    }

    expect(await screen.findByLabelText('导出内容')).toHaveValue('TEXT-plain');
    expect(exportBodies(stubbedFetch())).toEqual([
      {
        items: [
          { rel_path: 'a.mp4', url: 'https://dl.example/1' },
          { rel_path: 'sub/b.mkv', url: 'https://dl.example/2' },
        ],
        format: 'plain',
      },
    ]);
    // 时效提示上屏
    expect(screen.getByRole('dialog')).toHaveTextContent(EXPIRES_HINT);
  });

  it('点击切换五格式：各自调 export、文本上屏、aria-selected 迁移', async () => {
    renderDialog();
    await vi.waitFor(() =>
      expect(screen.getByLabelText('导出内容')).toHaveValue('TEXT-plain'),
    );
    const fetchMock = stubbedFetch();

    for (const format of FORMATS) {
      fireEvent.click(screen.getByRole('tab', { name: TAB_LABEL[format] }));
      await vi.waitFor(() =>
        expect(screen.getByLabelText('导出内容')).toHaveValue(`TEXT-${format}`),
      );
      expect(
        screen.getByRole('tab', { name: TAB_LABEL[format] }),
      ).toHaveAttribute('aria-selected', 'true');
    }
    expect(exportBodies(fetchMock).map((b) => b.format)).toEqual([...FORMATS]);
  });

  it('键盘模式：ArrowRight/ArrowLeft 切换且选中随焦点，Home/End 跳端点', async () => {
    renderDialog();
    await vi.waitFor(() =>
      expect(screen.getByLabelText('导出内容')).toHaveValue('TEXT-plain'),
    );

    const first = screen.getByRole('tab', { name: TAB_LABEL.plain });
    fireEvent.keyDown(first, { key: 'ArrowRight' });
    expect(screen.getByRole('tab', { name: TAB_LABEL.named })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    fireEvent.keyDown(
      screen.getByRole('tab', { name: TAB_LABEL.named }),
      { key: 'ArrowLeft' },
    );
    expect(first).toHaveAttribute('aria-selected', 'true');
    fireEvent.keyDown(first, { key: 'End' });
    expect(
      screen.getByRole('tab', { name: TAB_LABEL.motrix }),
    ).toHaveAttribute('aria-selected', 'true');
    fireEvent.keyDown(
      screen.getByRole('tab', { name: TAB_LABEL.motrix }),
      { key: 'Home' },
    );
    expect(first).toHaveAttribute('aria-selected', 'true');
    await vi.waitFor(() =>
      expect(screen.getByLabelText('导出内容')).toHaveValue('TEXT-plain'),
    );
  });

  it('加载中：复制禁用；完成后恢复', async () => {
    let release!: (v: unknown) => void;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        (_url: string | URL, init?: RequestInit) =>
          new Promise((resolve) => {
            release = resolve;
            void init;
          }),
      ),
    );
    renderDialog();

    // pending 中：复制不可用（无文本可复制）
    expect(screen.getByRole('button', { name: '复制' })).toBeDisabled();
    release({
      ok: true,
      status: 200,
      text: async () => 'TEXT-plain',
    });
    await vi.waitFor(() =>
      expect(screen.getByLabelText('导出内容')).toHaveValue('TEXT-plain'),
    );
    expect(screen.getByRole('button', { name: '复制' })).toBeEnabled();
  });

  it('复制：clipboard.writeText(文本) + toast 反馈', async () => {
    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    });
    renderDialog();
    await vi.waitFor(() =>
      expect(screen.getByLabelText('导出内容')).toHaveValue('TEXT-plain'),
    );

    fireEvent.click(screen.getByRole('button', { name: '复制' }));
    await vi.waitFor(() => expect(writeText).toHaveBeenCalledWith('TEXT-plain'));
    expect(toastShow).toHaveBeenCalledWith('已复制到剪贴板', 'success');
  });

  it('clipboard 不可用：失败 toast，不抛异常', async () => {
    Object.defineProperty(navigator, 'clipboard', {
      value: undefined,
      configurable: true,
    });
    renderDialog();
    await vi.waitFor(() =>
      expect(screen.getByLabelText('导出内容')).toHaveValue('TEXT-plain'),
    );

    fireEvent.click(screen.getByRole('button', { name: '复制' }));
    await vi.waitFor(() =>
      expect(toastShow).toHaveBeenCalledWith('复制失败，请手动复制', 'error'),
    );
  });

  it('export 非 2xx：role=alert 显示后端原因', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => ({
        ok: false,
        status: 400,
        json: async () => ({
          error: { code: 'invalid_links_request', message: '未知导出格式: xxx' },
        }),
      })),
    );
    renderDialog();

    expect(await screen.findByRole('alert')).toHaveTextContent('未知导出格式');
  });

  it('快速切换格式：旧格式的迟到响应不覆盖新格式文本（代际防护）', async () => {
    const fetchMock = stubbedFetch();
    // 第一次（plain）挂起不归；切到 named 后 plain 的迟到值到达
    let releasePlain!: (v: unknown) => void;
    fetchMock.mockImplementationOnce(
      (_url: string | URL, _init?: RequestInit) =>
        new Promise((resolve) => {
          releasePlain = resolve;
        }),
    );
    renderDialog();

    fireEvent.click(screen.getByRole('tab', { name: TAB_LABEL.named }));
    await vi.waitFor(() =>
      expect(screen.getByLabelText('导出内容')).toHaveValue('TEXT-named'),
    );

    // plain 迟到：不得覆盖 named 的文本
    releasePlain({ ok: true, status: 200, text: async () => 'TEXT-plain-stale' });
    await vi.waitFor(() =>
      expect(
        (screen.getByLabelText('导出内容') as HTMLTextAreaElement).value,
      ).toBe('TEXT-named'),
    );
  });
});

/** tab 文案 ↔ format 映射（与实现共享一份语义）。 */
const TAB_LABEL: Record<(typeof FORMATS)[number], string> = {
  plain: '纯文本',
  named: '带文件名',
  aria2: 'aria2',
  curl: 'curl',
  motrix: 'Motrix',
};

function stubbedFetch(): ReturnType<typeof vi.fn> {
  return vi.mocked(globalThis.fetch) as ReturnType<typeof vi.fn>;
}

/**
 * 样式层防回归（T10 真机验收②：长 URL 撑破 Dialog 面板）。
 * jsdom 不做 CSS 布局、断言不了计算样式，故对源 CSS 文本钉关键声明：
 * 容器收缩兜底 + textarea 全宽约束 + 长词断行，防止回归到 40rem 强制下限。
 */
describe('ExportDialog 样式防回归（文本框溢出）', () => {
  const cssText = readFileSync('src/components/export-dialog.css', 'utf8');

  it('容器 min-inline-size:0 兜底，不再强制 40rem 下限', () => {
    expect(cssText).toMatch(/\.export-dialog\s*\{[^}]*min-inline-size:\s*0/);
    expect(cssText).not.toMatch(/min-inline-size:\s*min\(40rem/);
  });

  it('textarea 全宽约束 + 长词断行', () => {
    expect(cssText).toMatch(
      /\.export-dialog__text\s*\{[^}]*max-inline-size:\s*100%/,
    );
    expect(cssText).toMatch(
      /\.export-dialog__text\s*\{[^}]*word-break:\s*break-all/,
    );
  });
});
