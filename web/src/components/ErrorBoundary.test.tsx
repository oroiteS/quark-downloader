// ErrorBoundary 行为钉死（T16 第三层：顶层兜底，杜绝白屏）：
// 1. 子组件渲染抛错 → 边界捕获，显示错误卡（role=alert）而非整树卸载白屏；
// 2. 错误卡含「重新加载」按钮，点击调 location.reload()（jsdom 未实现
//    navigation，以 stub 钉住调用本身）；
// 3. 正常子组件照常渲染，错误卡不出现（边界零侵入）；
// 手法：自绘 throwing 子组件；console.error 被 React 侵入式报错占用，
// 用例内静默避免噪音输出。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { ErrorBoundary } from './ErrorBoundary';

/** 渲染即抛错的哨兵子组件（仅在标 dirty 时抛，复用同一组件型）。 */
function Bomb({ dirty }: { dirty?: boolean }): never {
  if (dirty) {
    throw new Error('渲染期哨兵错误');
  }
  return null as never;
}

let consoleError: ReturnType<typeof vi.spyOn>;
let reload: ReturnType<typeof vi.fn>;

beforeEach(() => {
  consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
  // jsdom 的 location.reload 未实现（Not implemented: navigation），
  // 以可写 stub 替换整个 location 对象，钉住 reload 调用断言。
  reload = vi.fn();
  Object.defineProperty(window, 'location', {
    configurable: true,
    writable: true,
    value: { ...window.location, reload },
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('ErrorBoundary：顶层渲染错误兜底', () => {
  it('子组件渲染抛错：边界捕获显示错误卡（role=alert），不白屏', () => {
    render(
      <ErrorBoundary>
        <Bomb dirty />
      </ErrorBoundary>,
    );

    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent('页面出错了');
    expect(alert).toHaveTextContent('请重新加载页面');
  });

  it('错误卡「重新加载」按钮：点击调 location.reload()', () => {
    render(
      <ErrorBoundary>
        <Bomb dirty />
      </ErrorBoundary>,
    );

    fireEvent.click(screen.getByRole('button', { name: '重新加载' }));
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it('子组件正常：照常渲染不拦截，错误卡不出现', () => {
    render(
      <ErrorBoundary>
        <p data-testid="ok-child">正常内容</p>
      </ErrorBoundary>,
    );

    expect(screen.getByTestId('ok-child')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(consoleError).not.toHaveBeenCalled();
  });
});
