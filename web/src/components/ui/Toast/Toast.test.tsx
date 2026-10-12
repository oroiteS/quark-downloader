// Toast 行为钉死（简报 Step 1 + 前序交接）：toast.show 后消息出现在
// body 级 aria-live="polite" 容器；kind 落 BEM 类名；超时自动消退。
import { act } from 'react'; // React 18.3 起原生导出 act（RTL 16 的 re-export 在 vitest 下不可用）
import { afterEach, describe, expect, it, vi } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { toast, ToastHost } from './Toast';

// 直用 react 的 act 需显式声明 act 测试环境，否则 React 出 warning
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT =
  true;

afterEach(cleanup);

describe('Toast', () => {
  it('toast.show 后消息出现在 body 级 aria-live 容器（portal 单例）', () => {
    render(<ToastHost />);
    const region = document.querySelector('[aria-live="polite"]');
    expect(region).not.toBeNull();
    expect(region!.parentElement).toBe(document.body);
    act(() => {
      toast.show('已开始下载', 'info');
    });
    expect(region).toHaveTextContent('已开始下载');
  });

  it('kind 落到 BEM 类名', () => {
    render(<ToastHost />);
    act(() => {
      toast.show('任务失败', 'error');
      toast.show('已删除', 'success');
    });
    expect(screen.getByText('任务失败').className).toContain('toast--error');
    expect(screen.getByText('已删除').className).toContain('toast--success');
  });

  it('超时后自动消退', () => {
    vi.useFakeTimers();
    try {
      render(<ToastHost />);
      act(() => {
        toast.show('稍等', 'info');
      });
      expect(screen.getByText('稍等')).toBeInTheDocument();
      act(() => {
        vi.advanceTimersByTime(4_000);
      });
      expect(screen.queryByText('稍等')).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });
});
