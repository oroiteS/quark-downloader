// Dialog 行为钉死（简报 Step 1 + skill Modal 模式）：
// 打开 role="dialog"/aria-modal/标题关联且焦点入内；Tab 循环不出界；
// Esc 调 onClose；关闭后焦点归还先前聚焦元素。
import { afterEach, describe, expect, it, vi } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { Dialog } from './Dialog';

afterEach(cleanup);

describe('Dialog', () => {
  it('打开时 role=dialog + aria-modal + 标题经 aria-labelledby 关联，焦点入首个可聚焦元素', () => {
    render(
      <>
        <button>外部</button>
        <Dialog open onClose={() => {}} title="确认删除">
          <button>取消</button>
          <button>确定</button>
        </Dialog>
      </>,
    );
    const dialog = screen.getByRole('dialog');
    expect(dialog).toHaveAttribute('aria-modal', 'true');
    const labelledBy = dialog.getAttribute('aria-labelledby');
    expect(labelledBy).toBeTruthy();
    expect(document.getElementById(labelledBy!)).toHaveTextContent('确认删除');
    // 焦点陷阱第一步：打开即入内（首可聚焦 = “取消”）
    expect(document.activeElement).toBe(
      screen.getByRole('button', { name: '取消' }),
    );
  });

  it('open=false 时不渲染', () => {
    render(
      <Dialog open={false} onClose={() => {}} title="t">
        <button>x</button>
      </Dialog>,
    );
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('Tab 循环不出界：末元素 Tab 回首元素，首元素 Shift+Tab 回末元素', () => {
    render(
      <Dialog open onClose={() => {}} title="t">
        <button>第一</button>
        <button>第二</button>
        <button>第三</button>
      </Dialog>,
    );
    const first = screen.getByRole('button', { name: '第一' });
    const last = screen.getByRole('button', { name: '第三' });
    // 开场焦点在“第一”：Shift+Tab 应回绕到末元素
    fireEvent.keyDown(first, { key: 'Tab', shiftKey: true });
    expect(document.activeElement).toBe(last);
    // 从末元素 Tab 应回绕到首元素（不落到遮罩/外部）
    fireEvent.keyDown(last, { key: 'Tab' });
    expect(document.activeElement).toBe(first);
  });

  it('Esc 调用 onClose', () => {
    const onClose = vi.fn();
    render(
      <Dialog open onClose={onClose} title="t">
        <button>好</button>
      </Dialog>,
    );
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('关闭后焦点归还先前聚焦元素', () => {
    const { rerender } = render(
      <>
        <button>触发者</button>
        <Dialog open={false} onClose={() => {}} title="t">
          <button>好</button>
        </Dialog>
      </>,
    );
    const trigger = screen.getByRole('button', { name: '触发者' });
    trigger.focus();
    rerender(
      <>
        <button>触发者</button>
        <Dialog open onClose={() => {}} title="t">
          <button>好</button>
        </Dialog>
      </>,
    );
    expect(document.activeElement).not.toBe(trigger); // 焦点已入内
    rerender(
      <>
        <button>触发者</button>
        <Dialog open={false} onClose={() => {}} title="t">
          <button>好</button>
        </Dialog>
      </>,
    );
    expect(document.activeElement).toBe(trigger); // 归还
  });
});
