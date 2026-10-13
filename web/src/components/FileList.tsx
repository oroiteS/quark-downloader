/**
 * 通用文件列表（Task 6，Task 8 网盘页复用）：
 * - 数据源经 props 传入（条目数组 + 回调），不耦合 sid / parse 会话；
 * - 表格语义（thead/th scope），复选框 aria-label 含文件名（读屏可辨行）；
 * - 目录行点击导航（onOpenDir 缺省时目录名不可点）；
 * - 大小 1024 基格式化（formatSize 导出，Task 8 同源复用）；
 * - 目录勾选 = 整棵子树交后端展开，行内「含子目录」可见标注；
 * - 全选三态（全/无/部分 indeterminate），触控目标 ≥44px。
 */
import { useEffect, useRef } from 'react';
import type { FileEntry } from '../api/types';
import './filelist.css';

/** 大小格式化（1024 基；整数不补 .0，如 1.5 KB / 4 KB / 2 GB）。 */
export function formatSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'] as const;
  let value = bytes;
  let i = 0;
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024;
    i += 1;
  }
  const text = i === 0 ? String(value) : value.toFixed(1).replace(/\.0$/, '');
  return `${text} ${units[i]}`;
}

export interface FileListProps {
  /** 当前列表条目（父层决定来源：分享根/子目录/网盘）。 */
  files: readonly FileEntry[];
  /** 已勾选 fid 集合（受控；fid 全局唯一，跨目录保留）。 */
  checked: ReadonlySet<string>;
  /** 单行勾选切换。 */
  onToggle: (fid: string, next: boolean) => void;
  /** 全选/清空「当前列表」（父层合并语义，其他目录勾选不受影响）。 */
  onToggleAll: (next: boolean) => void;
  /** 目录行点击导航；缺省时目录名不可点。 */
  onOpenDir?: (entry: FileEntry) => void;
  /** 空列表文案。 */
  emptyText?: string;
}

export function FileList({
  files,
  checked,
  onToggle,
  onToggleAll,
  onOpenDir,
  emptyText = '此目录为空',
}: FileListProps) {
  const allRef = useRef<HTMLInputElement>(null);
  const checkedCount = files.filter((f) => checked.has(f.fid)).length;
  const allChecked = files.length > 0 && checkedCount === files.length;
  const someChecked = checkedCount > 0 && checkedCount < files.length;

  // indeterminate 是 DOM 属性而非 HTML attribute，只能经 ref 设置
  useEffect(() => {
    if (allRef.current) allRef.current.indeterminate = someChecked;
  }, [someChecked]);

  return (
    <div className="file-list">
      <table className="file-list__table" aria-label="文件列表">
        <thead>
          <tr>
            <th scope="col" className="file-list__col-check">
              <label className="file-list__check">
                <input
                  ref={allRef}
                  type="checkbox"
                  aria-label="全选"
                  checked={allChecked}
                  onChange={(e) => onToggleAll(e.target.checked)}
                />
              </label>
            </th>
            <th scope="col">名称</th>
            <th scope="col" className="file-list__col-size">
              大小
            </th>
          </tr>
        </thead>
        <tbody>
          {files.map((f) => (
            <tr key={f.fid}>
              <td className="file-list__col-check">
                <label className="file-list__check">
                  <input
                    type="checkbox"
                    aria-label={
                      f.dir
                        ? `选择 ${f.file_name}（含子目录）`
                        : `选择 ${f.file_name}`
                    }
                    checked={checked.has(f.fid)}
                    onChange={(e) => onToggle(f.fid, e.target.checked)}
                  />
                </label>
              </td>
              <td>
                <span className="file-list__name">
                  {f.dir ? (
                    <>
                      {onOpenDir ? (
                        <button
                          type="button"
                          className="file-list__dir"
                          onClick={() => onOpenDir(f)}
                        >
                          {f.file_name}
                        </button>
                      ) : (
                        <span>{f.file_name}</span>
                      )}
                      <span className="file-list__tag">含子目录</span>
                    </>
                  ) : (
                    <span>{f.file_name}</span>
                  )}
                </span>
              </td>
              <td className="file-list__col-size">
                {f.dir ? '—' : formatSize(f.size)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {files.length === 0 && <p className="file-list__empty">{emptyText}</p>}
    </div>
  );
}
