import { useEffect, useId, useRef } from 'react';
import type { KeyboardEvent, MouseEvent, ReactNode } from 'react';
import './dialog.css';

/** 焦点陷阱遍历集（skill Modal 模式选择器；disabled 在取集后过滤）。 */
const FOCUSABLE_SELECTOR =
  'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])';

export interface DialogProps {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  children: ReactNode;
}

/**
 * 模态对话框（skill Modal 模式）：
 * 打开→保存先前焦点、焦点入内；Tab 循环不出界；Esc/遮罩点击/关闭按钮关闭；
 * 关闭→焦点归还先前元素。遮罩用 --color-scrim（Task 1 备好）。
 *
 * T10 真机验收修复（三重关闭缺失）：
 * - Esc 监听提至 document 级（原绑 overlay，焦点漂出后失效——T2 deferred
 *   核销）；Escape 分支随之从 overlay onKeyDown 移除，防同一次按键双触发。
 * - 右上角关闭按钮（aria-label="关闭"，44px 触达，DOM 序在 children 之后
 *   =陷阱集末位，不改变「首焦点入首个业务控件」）。
 * - 遮罩 mousedown（target 为 overlay 本身才关——在面板内按下拖选到遮罩
 *   释放不误关）。
 */
export function Dialog({ open, onClose, title, children }: DialogProps) {
  const dialogRef = useRef<HTMLDivElement>(null);
  const previouslyFocused = useRef<HTMLElement | null>(null);
  const titleId = useId();

  useEffect(() => {
    if (!open) return;

    // 保存先前焦点 → 首焦点入内（无可聚焦子元素时兜底聚焦容器）
    previouslyFocused.current = document.activeElement as HTMLElement | null;
    const dialog = dialogRef.current;
    if (dialog) {
      const first =
        dialog.querySelector<HTMLElement>(FOCUSABLE_SELECTOR) ?? dialog;
      first.focus();
    }

    // 关闭/卸载 → 归还先前焦点
    return () => {
      previouslyFocused.current?.focus();
    };
  }, [open]);

  // Esc document 级：无论焦点落在何处（含漂出 overlay）都能关闭
  useEffect(() => {
    if (!open) return;
    const onKeyDown = (event: globalThis.KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        onClose();
      }
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [open, onClose]);

  if (!open) return null;

  // Tab 陷阱（Esc 已上移 document，此处不再处理）
  const handleKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key !== 'Tab') return;

    const dialog = dialogRef.current;
    if (!dialog) return;
    const focusables = Array.from(
      dialog.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR),
    ).filter((el) => !el.hasAttribute('disabled'));
    if (focusables.length === 0) {
      event.preventDefault();
      dialog.focus();
      return;
    }

    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    const active = document.activeElement;

    // 焦点漂出容器（理论上不该发生）→ 拉回首元素
    if (!dialog.contains(active)) {
      event.preventDefault();
      first.focus();
      return;
    }
    // 首元素 Shift+Tab → 回绕末元素；末元素 Tab → 回绕首元素
    if (event.shiftKey && active === first) {
      event.preventDefault();
      last.focus();
      return;
    }
    if (!event.shiftKey && active === last) {
      event.preventDefault();
      first.focus();
    }
  };

  // 遮罩点击关闭在 mousedown 判定：从面板内按下拖选到遮罩释放不误关
  const handleOverlayMouseDown = (event: MouseEvent<HTMLDivElement>) => {
    if (event.target === event.currentTarget) {
      onClose();
    }
  };

  return (
    <div
      className="dialog-overlay"
      onKeyDown={handleKeyDown}
      onMouseDown={handleOverlayMouseDown}
    >
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        className="dialog"
      >
        <h2 id={titleId} className="dialog__title">
          {title}
        </h2>
        {children}
        <button
          type="button"
          className="dialog__close"
          aria-label="关闭"
          onClick={onClose}
        >
          <span aria-hidden="true">×</span>
        </button>
      </div>
    </div>
  );
}
