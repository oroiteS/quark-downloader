/**
 * 网盘页（Task 8，/drive 路由）：网盘目录浏览与勾选下载。
 *
 * 页面流（简报钉死）：
 * - 挂载即 GET /api/drive/files?pdir_fid=0（根 fid="0"）渲染根列表；
 * - 目录行进入子目录（GET pdir_fid=fid）+ 面包屑逐级回退（nav>ol 语义，
 *   当前层 aria-current="location"，与解析页 G10 同型）；
 * - 勾选（fid 集合跨目录保留；目录勾选=整棵子树交后端展开，FileList
 *   「含子目录」标注）→「下载选中」Dialog：目标子目录同 Task 6（默认空
 *   =全局 destDir）；mode radio 组（fieldset/legend）默认 a，B 标注
 *   「边下边播」（D5）→ POST /api/tasks {source:"drive", fids, dest, mode}；
 * - 提交中禁用按钮（Button loading=disabled+aria-busy）禁止重复提交；
 *   skipped 逐条 toast（创建链 dest_busy 进 skipped 条目，tasks.go:307
 *   实测——「同一文件正在下载中」同路径互斥同样用户可见）；HTTP 错误
 *   （含 409 dest_busy）→ Toast 文案取后端 message（简报钉死），Dialog
 *   保留可重试。
 *
 * 边界：会话过期跳 /login 由守卫负责（App.tsx 三态守卫），本页不订阅；
 * 提交成功不跳 /tasks（简报未钉跳转，T6 的跳转是其简报明示业务）——
 * 关 Dialog 清勾选，防同批误重复提交。
 * 目录导航代际防护（genRef）：快速切换目录时旧响应不覆盖新视图。
 */
import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Button, Dialog, Input, Spinner, toast } from '../components/ui';
import { FileList } from '../components/FileList';
import { api } from '../api/client';
import { ApiError } from '../api/types';
import type { FileEntry } from '../api/types';
import './drive.css';

/** 面包屑层级：fid + 目录名（空栈=根）。 */
interface Crumb {
  fid: string;
  name: string;
}

/** 根目录 fid（后端钉死："0"）。 */
const ROOT_FID = '0';

const ROOT_CRUMB = '网盘根目录';

/** skippedItem 响应元素（tasks.go 逐字：fid/name/reason）。 */
interface SkippedItem {
  fid: string;
  name: string;
  reason: string;
}

/** POST /api/tasks 响应（创建链恒 200：task_ids + skipped）。 */
interface CreateTasksResult {
  task_ids: string[];
  skipped: SkippedItem[];
}

export function Drive() {
  const [files, setFiles] = useState<FileEntry[]>([]);
  const [stack, setStack] = useState<Crumb[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [checked, setChecked] = useState<ReadonlySet<string>>(new Set());

  const [downloadOpen, setDownloadOpen] = useState(false);
  const [dest, setDest] = useState('');
  const [mode, setMode] = useState<'a' | 'b'>('a');
  const [submitting, setSubmitting] = useState(false);

  const genRef = useRef(0);

  /** 拉取目录：fid=目标层，nextStack=该层对应面包屑栈（导航代际防护）。 */
  async function loadDir(fid: string, nextStack: Crumb[]): Promise<void> {
    const gen = ++genRef.current;
    setLoading(true);
    setLoadError(null);
    try {
      const r = await api.get<{ files: FileEntry[] }>(
        `/api/drive/files?pdir_fid=${encodeURIComponent(fid)}`,
      );
      if (genRef.current !== gen) return;
      setFiles(r.files);
      setStack(nextStack);
    } catch (err) {
      if (genRef.current !== gen) return;
      // 401 未登录 / 502 拉取失败按 apiFetch 抛错常规处理：后端文案上屏
      setLoadError(
        err instanceof ApiError ? err.message : '目录加载失败，请稍后重试',
      );
    } finally {
      if (genRef.current === gen) setLoading(false);
    }
  }

  useEffect(() => {
    void loadDir(ROOT_FID, []);
    // 卸载使在途响应作废（旧 gen 不再写状态）
    return () => {
      genRef.current += 1;
    };
  }, []);

  /** 进入子目录：以其 fid 拉子列表并入栈（面包屑增长）。 */
  function enterDir(entry: FileEntry): void {
    void loadDir(entry.fid, [...stack, { fid: entry.fid, name: entry.file_name }]);
  }

  /** 面包屑回跳（idx=-1 为根）：截断栈到该层并重取列表。 */
  function goTo(idx: number): void {
    const fid = idx < 0 ? ROOT_FID : stack[idx].fid;
    void loadDir(fid, stack.slice(0, idx + 1));
  }

  function toggle(fid: string, next: boolean): void {
    setChecked((prev) => {
      const set = new Set(prev);
      if (next) set.add(fid);
      else set.delete(fid);
      return set;
    });
  }

  /** 全选/清空当前列表（其他目录已勾选项不受影响）。 */
  function toggleAll(next: boolean): void {
    setChecked((prev) => {
      const set = new Set(prev);
      for (const f of files) {
        if (next) set.add(f.fid);
        else set.delete(f.fid);
      }
      return set;
    });
  }

  function openDownload(): void {
    setDownloadOpen(true);
  }

  async function submitDownload(e: FormEvent): Promise<void> {
    e.preventDefault();
    if (submitting) return;
    setSubmitting(true);
    try {
      const r = await api.post<CreateTasksResult>('/api/tasks', {
        source: 'drive',
        fids: [...checked],
        dest,
        mode,
      });
      // skipped 逐条反馈（同 T6；创建链 dest_busy 也走此通道用户可见）
      for (const s of r.skipped) {
        toast.show(`已跳过：${s.name}（${s.reason}）`, 'info');
      }
      setChecked(new Set());
      setDownloadOpen(false);
    } catch (err) {
      // T10 真机复验二轮：非 ApiError（fetch 层 reject）时 console 留
      // 原始错误现场（Toast 文案语义不变，简报钉死取后端 message）。
      console.error('任务提交失败', err);
      // 简报钉死：错误（含 409 dest_busy）Toast 文案取后端 message
      toast.show(
        err instanceof ApiError ? err.message : '任务提交失败，请稍后重试',
        'error',
      );
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <section className="drive">
      <h1>网盘</h1>

      <nav aria-label="目录路径" className="drive__crumbs">
        <ol>
          <li>
            {stack.length === 0 ? (
              <span className="drive__crumb-current" aria-current="location">
                {ROOT_CRUMB}
              </span>
            ) : (
              <button
                type="button"
                className="drive__crumb-btn"
                onClick={() => goTo(-1)}
              >
                {ROOT_CRUMB}
              </button>
            )}
          </li>
          {stack.map((c, i) => (
            <li key={c.fid}>
              {i === stack.length - 1 ? (
                <span className="drive__crumb-current" aria-current="location">
                  {c.name}
                </span>
              ) : (
                <button
                  type="button"
                  className="drive__crumb-btn"
                  onClick={() => goTo(i)}
                >
                  {c.name}
                </button>
              )}
            </li>
          ))}
        </ol>
      </nav>

      {loadError && (
        <p className="drive__error" role="alert">
          {loadError}
        </p>
      )}

      {loading ? (
        <p className="drive__busy" role="status">
          <Spinner />
          正在加载目录…
        </p>
      ) : (
        <FileList
          files={files}
          checked={checked}
          onToggle={toggle}
          onToggleAll={toggleAll}
          onOpenDir={enterDir}
          tableLabel="网盘文件列表"
        />
      )}

      <div className="drive__actions">
        <p className="drive__selected" aria-live="polite">
          已选 {checked.size} 项
        </p>
        <Button
          variant="secondary"
          disabled={checked.size === 0}
          onClick={openDownload}
        >
          下载选中
        </Button>
      </div>

      <Dialog
        open={downloadOpen}
        onClose={() => setDownloadOpen(false)}
        title="下载选中项"
      >
        <form className="drive__dialog-form" onSubmit={(e) => void submitDownload(e)}>
          <Input
            label="目标子目录"
            value={dest}
            onChange={(e) => setDest(e.target.value)}
            hint="留空则保存到全局下载目录"
          />
          <fieldset className="drive__mode">
            <legend className="drive__mode-legend">下载模式</legend>
            <label className="drive__mode-option">
              <input
                type="radio"
                name="drive-mode"
                value="a"
                checked={mode === 'a'}
                onChange={() => setMode('a')}
              />
              A（标准下载）
            </label>
            <label className="drive__mode-option">
              <input
                type="radio"
                name="drive-mode"
                value="b"
                checked={mode === 'b'}
                onChange={() => setMode('b')}
              />
              B（边下边播）
            </label>
          </fieldset>
          <div className="drive__dialog-actions">
            <Button
              type="button"
              variant="ghost"
              onClick={() => setDownloadOpen(false)}
            >
              取消
            </Button>
            <Button type="submit" loading={submitting}>
              开始下载
            </Button>
          </div>
        </form>
      </Dialog>
    </section>
  );
}
