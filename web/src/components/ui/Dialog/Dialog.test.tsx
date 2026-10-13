// Dialog 行为钉死（简报 Step 1 + skill Modal 模式 + T10 真机验收修复）：
// 打开 role="dialog"/aria-modal/标题关联且焦点入内；Tab 循环不出界；
// Esc 关闭（document 级监听——焦点漂出 overlay 仍生效，T2 deferred 核销）；
// 右上角关闭按钮（aria-label="关闭"）点击关闭；遮罩 mousedown 关闭；
// 关闭后焦点归还先前聚焦元素。
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

  it('Tab 循环不出界：首元素 Shift+Tab 回绕关闭按钮，关闭按钮 Tab 回首元素', () => {
    render(
      <Dialog open onClose={() => {}} title="t">
        <button>第一</button>
        <button>第二</button>
        <button>第三</button>
      </Dialog>,
    );
    const first = screen.getByRole('button', { name: '第一' });
    // 关闭按钮 DOM 序在 children 之后 = 陷阱集最后一个可聚焦元素
    const close = screen.getByRole('button', { name: '关闭' });
    // 开场焦点在“第一”：Shift+Tab 应回绕到末元素（关闭按钮）
    fireEvent.keyDown(first, { key: 'Tab', shiftKey: true });
    expect(document.activeElement).toBe(close);
    // 从末元素 Tab 应回绕到首元素（不落到遮罩/外部）
    fireEvent.keyDown(close, { key: 'Tab' });
    expect(document.activeElement).toBe(first);
  });

  it('Esc 调用 onClose（焦点在 dialog 内）', () => {
    const onClose = vi.fn();
    render(
      <Dialog open onClose={onClose} title="t">
        <button>好</button>
      </Dialog>,
    );
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('焦点漂出到 body 时 Esc 仍关闭（document 级监听，T2 deferred 核销）', () => {
    const onClose = vi.fn();
    render(
      <Dialog open onClose={onClose} title="t">
        <button>好</button>
      </Dialog>,
    );
    // 模拟焦点漂出：body 持焦（不聚焦 dialog 内任何元素），事件直接派发到 body
    expect(document.activeElement).not.toBe(screen.getByRole('dialog'));
    fireEvent.keyDown(document.body, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('右上角关闭按钮：aria-label="关闭"，点击调 onClose', () => {
    const onClose = vi.fn();
    render(
      <Dialog open onClose={onClose} title="t">
        <button>好</button>
      </Dialog>,
    );
    const close = screen.getByRole('button', { name: '关闭' });
    expect(close).toBeInTheDocument();
    fireEvent.click(close);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('遮罩 mousedown 关闭：target 为 overlay 本身才关，面板内按下不关', () => {
    const onClose = vi.fn();
    render(
      <Dialog open onClose={onClose} title="t">
        <button>好</button>
      </Dialog>,
    );
    const dialog = screen.getByRole('dialog');
    const overlay = dialog.parentElement as HTMLElement;
    // 面板内按下（含从面板拖选到遮罩释放的场景）：不关
    fireEvent.mouseDown(dialog);
    expect(onClose).not.toHaveBeenCalled();
    // 遮罩本体按下：关
    fireEvent.mouseDown(overlay);
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
