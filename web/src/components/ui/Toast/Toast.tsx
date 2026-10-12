import { useSyncExternalStore } from 'react';
import { createPortal } from 'react-dom';
import './toast.css';

export type ToastKind = 'info' | 'error' | 'success';

/** 自动消退时长：轻提示无操作，4s 内读屏可完成播报。 */
const AUTO_DISMISS_MS = 4000;

interface ToastItem {
  id: number;
  message: string;
  kind: ToastKind;
}

/* store 触发式：模块级单例 store，任何处可调 toast.show()；
   ToastHost 订阅并经 portal 挂到 body 级 aria-live 区（前序交接④）。 */
let items: ToastItem[] = [];
let nextId = 0;
const listeners = new Set<() => void>();

function emit(): void {
  for (const listener of listeners) listener();
}

export const toast = {
  /** 弹一条轻提示，AUTO_DISMISS_MS 后自动消退。 */
  show(message: string, kind: ToastKind = 'info'): void {
    const id = ++nextId;
    items = [...items, { id, message, kind }];
    emit();
    setTimeout(() => {
      items = items.filter((item) => item.id !== id);
      emit();
    }, AUTO_DISMISS_MS);
  },

  /** useSyncExternalStore 订阅入口（引用稳定）。 */
  subscribe(listener: () => void): () => void {
    listeners.add(listener);
    return () => {
      listeners.delete(listener);
    };
  },

  /** 快照：仅在 show/消退时替换数组引用，保证引用稳定不炸渲染。 */
  getSnapshot(): ToastItem[] {
    return items;
  },
};

/** body 级 aria-live="polite" 单例容器：应用根挂载一次（Task 4 起接线）。 */
export function ToastHost() {
  const current = useSyncExternalStore(toast.subscribe, toast.getSnapshot);

  return createPortal(
    <div className="toast-region" aria-live="polite">
      {current.map((item) => (
        <div key={item.id} className={`toast toast--${item.kind}`}>
          {item.message}
        </div>
      ))}
    </div>,
    document.body,
  );
}
