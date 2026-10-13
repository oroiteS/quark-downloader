/**
 * 解析页（Task 6，/ 路由）：粘贴解析 → 目录导航 → 勾选下载。
 *
 * 页面流（简报钉死）：
 * - 粘贴框（原生 textarea，T5 先例：分享文本常含多行口令，ui 无 Textarea）
 *   → POST share/parse → FileList 根列表；
 * - 目录行进入子目录（GET share/files?pdir_fid=fid）+ 面包屑返回（G10）；
 * - 勾选（fid 全局集合跨目录保留；目录勾选=整棵子树交后端展开，FileList
 *   标注「含子目录」）→「下载选中」Dialog（dest 留空=全局 destDir）→
 *   POST tasks → skipped 逐条 toast → 跳 /tasks（简报明示的业务跳转）；
 * - kouling:true 响应显示 hint 不报错。
 *
 * 边界（T4 交接）：会话过期跳 /login 由守卫负责，本页不订阅过期；
 * 草稿写 session.pendingParseText（Focus #3：跳登录不丢）。
 * 目录导航代际防护（genRef）：快速切换目录时旧响应不覆盖新视图。
 */
import { useId, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { useNavigate } from 'react-router-dom';
import { Button, Dialog, Input, Spinner, toast } from '../components/ui';
import { FileList } from '../components/FileList';
import {
  createShareTasks,
  isKoulingParse,
  listShareFiles,
  parseShareText,
} from '../api/share';
import { ApiError } from '../api/types';
import type { FileEntry, ParseResult } from '../api/types';
import { useSession } from '../stores/session';
import './parse.css';

/** 面包屑层级：fid + 目录名（空栈=根）。 */
interface Crumb {
  fid: string;
  name: string;
}

const ROOT_CRUMB = '分享根目录';

export function Parse() {
  const navigate = useNavigate();
  const [text, setText] = useState(() => useSession.getState().pendingParseText);
  const [parsing, setParsing] = useState(false);
  const [parseError, setParseError] = useState<string | null>(null);
  const [koulingHint, setKoulingHint] = useState<string | null>(null);
  const [result, setResult] = useState<ParseResult | null>(null);

  const [files, setFiles] = useState<FileEntry[]>([]);
  const [stack, setStack] = useState<Crumb[]>([]);
  const [loadingFiles, setLoadingFiles] = useState(false);
  const [filesError, setFilesError] = useState<string | null>(null);
  const [checked, setChecked] = useState<ReadonlySet<string>>(new Set());

  const [downloadOpen, setDownloadOpen] = useState(false);
  const [dest, setDest] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [downloadError, setDownloadError] = useState<string | null>(null);

  const errorId = useId();
  const genRef = useRef(0);

  function changeText(value: string): void {
    setText(value);
    // Focus #3：草稿常驻内存，会话过期跳登录后回填
    useSession.getState().setPendingParseText(value);
  }

  async function submitParse(e: FormEvent): Promise<void> {
    e.preventDefault();
    const value = text.trim();
    if (!value) {
      setParseError('请粘贴分享链接');
      return;
    }
    setParsing(true);
    setParseError(null);
    setKoulingHint(null);
    try {
      const r = await parseShareText(value);
      if (isKoulingParse(r)) {
        // 纯口令场景：显示 hint 不报错（控制者钉死）
        setKoulingHint(r.hint);
        setResult(null);
        return;
      }
      setResult(r);
      setFiles(r.files);
      setStack([]);
      setChecked(new Set());
      setFilesError(null);
    } catch (err) {
      setParseError(
        err instanceof ApiError ? err.message : '解析失败，请稍后重试',
      );
    } finally {
      setParsing(false);
    }
  }

  /** 进入子目录：以其 fid 拉子列表并入栈（面包屑增长）。 */
  async function enterDir(entry: FileEntry): Promise<void> {
    if (!result) return;
    const gen = ++genRef.current;
    setLoadingFiles(true);
    setFilesError(null);
    try {
      const r = await listShareFiles(result.sid, entry.fid);
      if (genRef.current !== gen) return;
      setFiles(r.files);
      setStack((s) => [...s, { fid: entry.fid, name: entry.file_name }]);
    } catch (err) {
      if (genRef.current !== gen) return;
      // sid 失效 404：后端文案含「请重新解析」，原列表保留
      setFilesError(
        err instanceof ApiError ? err.message : '目录加载失败，请稍后重试',
      );
    } finally {
      if (genRef.current === gen) setLoadingFiles(false);
    }
  }

  /** 面包屑回跳（idx=-1 为根）：截断栈到该层并重取列表。 */
  async function goTo(idx: number): Promise<void> {
    if (!result) return;
    const gen = ++genRef.current;
    const fid = idx < 0 ? '' : stack[idx].fid;
    setLoadingFiles(true);
    setFilesError(null);
    try {
      const r = await listShareFiles(result.sid, fid);
      if (genRef.current !== gen) return;
      setFiles(r.files);
      setStack((s) => s.slice(0, idx + 1));
    } catch (err) {
      if (genRef.current !== gen) return;
      setFilesError(
        err instanceof ApiError ? err.message : '目录加载失败，请稍后重试',
      );
    } finally {
      if (genRef.current === gen) setLoadingFiles(false);
    }
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
    setDownloadError(null);
    setDownloadOpen(true);
  }

  async function submitDownload(e: FormEvent): Promise<void> {
    e.preventDefault();
    if (!result) return;
    setSubmitting(true);
    setDownloadError(null);
    try {
      const r = await createShareTasks(result.sid, [...checked], dest);
      // skipped 逐条反馈（简报钉死），随后跳 /tasks
      for (const s of r.skipped) {
        toast.show(`已跳过：${s.name}（${s.reason}）`, 'info');
      }
      navigate('/tasks');
    } catch (err) {
      // T10 真机复验二轮：成功路径曾误报兜底文案而真因不可见——
      // 非 ApiError（fetch 层 reject）时 console 留原始错误现场，
      // 兜底文案不变。
      console.error('任务提交失败', err);
      setDownloadError(
        err instanceof ApiError ? err.message : '任务提交失败，请稍后重试',
      );
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <section className="parse">
      <h1>解析</h1>

      <form className="parse__form" onSubmit={(e) => void submitParse(e)}>
        <label className="parse__label" htmlFor="parse-text">
          分享链接文本
        </label>
        <textarea
          id="parse-text"
          className="parse__textarea"
          rows={4}
          value={text}
          onChange={(e) => changeText(e.target.value)}
          placeholder="粘贴分享链接（可含提取码/口令）"
          aria-invalid={parseError ? true : undefined}
          aria-describedby={parseError ? errorId : undefined}
        />
        {parseError && (
          <p id={errorId} className="parse__error" role="alert">
            {parseError}
          </p>
        )}
        <Button type="submit" loading={parsing}>
          解析
        </Button>
      </form>

      {koulingHint && (
        <p className="parse__kouling" role="status">
          {koulingHint}
        </p>
      )}

      {result && (
        <>
          <nav aria-label="目录路径" className="parse__crumbs">
            <ol>
              <li>
                {stack.length === 0 ? (
                  <span className="parse__crumb-current" aria-current="location">
                    {ROOT_CRUMB}
                  </span>
                ) : (
                  <button
                    type="button"
                    className="parse__crumb-btn"
                    onClick={() => void goTo(-1)}
                  >
                    {ROOT_CRUMB}
                  </button>
                )}
              </li>
              {stack.map((c, i) => (
                <li key={c.fid}>
                  {i === stack.length - 1 ? (
                    <span className="parse__crumb-current" aria-current="location">
                      {c.name}
                    </span>
                  ) : (
                    <button
                      type="button"
                      className="parse__crumb-btn"
                      onClick={() => void goTo(i)}
                    >
                      {c.name}
                    </button>
                  )}
                </li>
              ))}
            </ol>
          </nav>

          {filesError && (
            <p className="parse__error" role="alert">
              {filesError}
            </p>
          )}

          {loadingFiles ? (
            <p className="parse__busy" role="status">
              <Spinner />
              正在加载目录…
            </p>
          ) : (
            <FileList
              files={files}
              checked={checked}
              onToggle={toggle}
              onToggleAll={toggleAll}
              onOpenDir={(f) => void enterDir(f)}
            />
          )}

          <div className="parse__actions">
            <p className="parse__selected" aria-live="polite">
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
        </>
      )}

      <Dialog
        open={downloadOpen}
        onClose={() => setDownloadOpen(false)}
        title="下载选中项"
      >
        <form className="parse__dialog-form" onSubmit={(e) => void submitDownload(e)}>
          <Input
            label="目标子目录"
            value={dest}
            onChange={(e) => setDest(e.target.value)}
            hint="留空则保存到全局下载目录"
          />
          {downloadError && (
            <p className="parse__error" role="alert">
              {downloadError}
            </p>
          )}
          <div className="parse__dialog-actions">
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
