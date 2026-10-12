import { useEffect, useId, useRef } from 'react';
import type { KeyboardEvent, ReactNode } from 'react';
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
 * 打开→保存先前焦点、焦点入内；Tab 循环不出界；Esc 关闭；
 * 关闭→焦点归还先前元素。遮罩用 --color-scrim（Task 1 备好）。
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

  if (!open) return null;

  const handleKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === 'Escape') {
      event.preventDefault();
      onClose();
      return;
    }
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

  return (
    <div className="dialog-overlay" onKeyDown={handleKeyDown}>
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
      </div>
    </div>
  );
}
