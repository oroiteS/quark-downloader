/**
 * 导出直链对话框（Task 6）：五格式 tab + 文本上屏 + 复制 + 时效提示。
 *
 * - 五格式枚举与后端 export.go 逐字对齐（plain/named/aria2/curl/motrix）；
 *   简报钉死「tab 切换各调 export」——每次切换都 POST /api/links/export 并
 *   以 res.text() 纯文本上屏（T3 账本：apiFetch 2xx 硬 res.json() 会崩）。
 * - rel_path 由前端组装：links 响应条目无 rel_path 字段，根列表=file_name。
 * - ARIA tabs 模式：tablist/tab/tabpanel + aria-selected + 方向键/Home/End
 *   （选中随焦点移动，roving focus）；容器复用 ui Dialog 的焦点陷阱。
 * - 代际防护（genRef）：快速切换格式时旧格式迟到响应不覆盖新文本。
 */
import { useEffect, useId, useRef, useState } from 'react';
import type { KeyboardEvent } from 'react';
import { Button, Dialog, toast } from './ui';
import { exportLinksText } from '../api/share';
import type { ExportFormat } from '../api/share';
import './export-dialog.css';

export interface ExportDialogProps {
  open: boolean;
  onClose: () => void;
  /** 直链条目（file_name + url；rel_path 由本组件以 file_name 组装）。 */
  items: ReadonlyArray<{ file_name: string; url: string }>;
  /** 后端 expires_hint 时效文案，原样上屏（语义：尽快使用/失效重取）。 */
  expiresHint?: string;
}

/** tab 文案 ↔ 导出格式（顺序 = 默认 tab 序 = export.go 枚举序）。 */
const TABS: ReadonlyArray<{ format: ExportFormat; label: string }> = [
  { format: 'plain', label: '纯文本' },
  { format: 'named', label: '带文件名' },
  { format: 'aria2', label: 'aria2' },
  { format: 'curl', label: 'curl' },
  { format: 'motrix', label: 'Motrix' },
];

export function ExportDialog({
  open,
  onClose,
  items,
  expiresHint,
}: ExportDialogProps) {
  const [format, setFormat] = useState<ExportFormat>('plain');
  const [text, setText] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const genRef = useRef(0);
  const tablistId = useId();
  const tabRefs = useRef<Partial<Record<ExportFormat, HTMLButtonElement | null>>>(
    {},
  );

  // open 与格式变化都重新取（简报：切换各调 export）；卸载/重入以代际丢弃旧响应
  useEffect(() => {
    if (!open) return;
    const gen = ++genRef.current;
    setLoading(true);
    setError(null);
    setText('');
    exportLinksText(
      items.map((i) => ({ rel_path: i.file_name, url: i.url })),
      format,
    )
      .then((t) => {
        if (genRef.current === gen) setText(t);
      })
      .catch((e) => {
        if (genRef.current === gen) {
          setError(e instanceof Error ? e.message : '导出失败，请稍后重试');
        }
      })
      .finally(() => {
        if (genRef.current === gen) setLoading(false);
      });
  }, [open, format, items]);

  function selectFormat(next: ExportFormat): void {
    setFormat(next);
    tabRefs.current[next]?.focus();
  }

  // ARIA tabs 键盘模式：以事件所在 tab 为基准移动（选中随焦点）
  function handleTabKeyDown(
    e: KeyboardEvent<HTMLButtonElement>,
    current: ExportFormat,
  ): void {
    const idx = TABS.findIndex((t) => t.format === current);
    let next = -1;
    switch (e.key) {
      case 'ArrowRight':
        next = (idx + 1) % TABS.length;
        break;
      case 'ArrowLeft':
        next = (idx - 1 + TABS.length) % TABS.length;
        break;
      case 'Home':
        next = 0;
        break;
      case 'End':
        next = TABS.length - 1;
        break;
      default:
        return;
    }
    e.preventDefault();
    selectFormat(TABS[next].format);
  }

  async function copy(): Promise<void> {
    if (!navigator.clipboard) {
      toast.show('复制失败，请手动复制', 'error');
      return;
    }
    try {
      await navigator.clipboard.writeText(text);
      toast.show('已复制到剪贴板', 'success');
    } catch {
      toast.show('复制失败，请手动复制', 'error');
    }
  }

  const activeTabId = `${tablistId}-${format}`;

  return (
    <Dialog open={open} onClose={onClose} title="导出直链">
      <div className="export-dialog">
        <div role="tablist" aria-label="导出格式" className="export-dialog__tabs" id={tablistId}>
          {TABS.map((t) => (
            <button
              key={t.format}
              ref={(el) => {
                tabRefs.current[t.format] = el;
              }}
              type="button"
              role="tab"
              id={`${tablistId}-${t.format}`}
              aria-selected={t.format === format}
              aria-controls={`${tablistId}-panel`}
              tabIndex={t.format === format ? 0 : -1}
              className="export-dialog__tab"
              onClick={() => selectFormat(t.format)}
              onKeyDown={(e) => handleTabKeyDown(e, t.format)}
            >
              {t.label}
            </button>
          ))}
        </div>

        <div
          role="tabpanel"
          id={`${tablistId}-panel`}
          aria-labelledby={activeTabId}
          aria-busy={loading}
          className="export-dialog__body"
        >
          <textarea
            className="export-dialog__text"
            aria-label="导出内容"
            readOnly
            value={text}
            rows={10}
            spellCheck={false}
          />
          {error && (
            <p className="export-dialog__error" role="alert">
              {error}
            </p>
          )}
          {expiresHint && <p className="export-dialog__hint">{expiresHint}</p>}
          <div className="export-dialog__actions">
            <Button
              variant="secondary"
              disabled={!text}
              onClick={() => void copy()}
            >
              复制
            </Button>
          </div>
        </div>
      </div>
    </Dialog>
  );
}
