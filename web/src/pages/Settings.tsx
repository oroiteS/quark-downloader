/**
 * 设置页（Task 9，D10/D7/G5/G13；2025 前端重构：退出登录迁至左下角用户
 * 菜单——AccountBadge，本页不再含账号区）。
 *
 * - 数据流：挂载 GET /api/settings 四字段回显；「保存」PUT /api/settings
 *   **全量四字段**（Review Focus #2：仅改主题也齐发 destDir/threads/max 原值，
 *   后端 PUT 为全量替换语义）。校验全在后端（webapi/settings.go：destDir
 *   非空/threads 1–512/max 1–8/themeLock 枚举）——本页不做空值拦截，
 *   400 invalid_settings 的 message 经表单级 role=alert 回显（统一通道，
 *   不散落字段级文案，前端无字段级校验规则可复述）；仅一处明知标注：
 *   保存失败且 destDir 为空时对下载目录 Input 置 aria-invalid。
 * - landmark：页面根元素用 <section>（非 main）——AppShell 已有
 *   <main id="main-content"> 作 skip link 落点，避免 main 嵌套 main
 *   （与 Parse/Tasks/Drive 同壳先例一致）。
 * - 主题（D7）：radio 三选切换即 applyTheme 本地即时生效，不等保存；
 *   跟随分支（""）返回的 matchMedia 监听卸载函数收进 ref，切换/卸载时
 *   先清，避免 listener 堆积。挂载回显不 apply（当前主题由 main.tsx
 *   启动编排负责，页面不重复覆盖）。
 * - threads 缺省渲染 32（G5）：GET 值不在档位枚举时回退 32 展示，
 *   保存按展示值上送（展示即提交）。
 */
import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Button, Card, Input, Spinner, toast } from '../components/ui';
import { api } from '../api/client';
import { ApiError } from '../api/types';
import type { PickDirResult } from '../api/types';
import { applyTheme } from '../theme';
import type { ThemeLock } from '../theme';
import './settings.css';

/** 后端 Settings 契约（webapi/settings.go json tag 逐字；PUT 同形全量替换）。 */
interface SettingsDTO {
  destDir: string;
  threads: number;
  maxConcurrentTasks: number;
  themeLock: ThemeLock;
}

/** 并发档位（G5，简报钉死六档）。 */
const THREAD_STEPS = [16, 32, 64, 128, 256, 512];
/** 同时任务数档位（G13，简报钉死 1–8）。 */
const MAX_TASK_STEPS = [1, 2, 3, 4, 5, 6, 7, 8];

/** 主题三选（D7）。"" = 跟随系统。 */
const THEME_OPTIONS: { value: ThemeLock; label: string }[] = [
  { value: '', label: '跟随系统' },
  { value: 'light', label: '浅色' },
  { value: 'dark', label: '深色' },
];

export function Settings() {
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [destDir, setDestDir] = useState('');
  const [threads, setThreads] = useState(32);
  const [maxTasks, setMaxTasks] = useState(1);
  const [themeLock, setThemeLock] = useState<ThemeLock>('');
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  // 目录选择器（计划外 Task 11）：请求期间按钮 loading（Button 的
  // disabled||loading 同源防竞态点击）；回填不自动保存。
  const [picking, setPicking] = useState(false);
  // 跟随分支的 matchMedia 监听卸载函数（D7）：切换前清旧、卸载兜底清。
  const themeCleanupRef = useRef<() => void>(() => {});

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const s = await api.get<SettingsDTO>('/api/settings');
        if (cancelled) return;
        setDestDir(s.destDir);
        // G5：不在档位枚举（含缺省/非法值）→ 缺省渲染 32
        setThreads(THREAD_STEPS.includes(s.threads) ? s.threads : 32);
        setMaxTasks(
          s.maxConcurrentTasks >= 1 && s.maxConcurrentTasks <= 8
            ? s.maxConcurrentTasks
            : 1,
        );
        setThemeLock(s.themeLock);
      } catch (e) {
        if (!cancelled) {
          setLoadError(
            e instanceof ApiError ? e.message : '设置加载失败，请稍后重试',
          );
        }
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
      themeCleanupRef.current?.();
    };
  }, []);

  // D7：切换即本地即时生效（不必等保存）。
  function handleThemeLock(lock: ThemeLock): void {
    setThemeLock(lock);
    themeCleanupRef.current?.();
    themeCleanupRef.current = applyTheme(lock);
  }

  // 全量语义（Review Focus #2）：四字段全部当前值齐发。
  async function handleSave(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    setSaveError(null);
    setSaving(true);
    try {
      await api.put<SettingsDTO>('/api/settings', {
        destDir,
        threads,
        maxConcurrentTasks: maxTasks,
        themeLock,
      });
      toast.show('设置已保存', 'success');
    } catch (e) {
      // 400 invalid_settings（空 destDir 等）：回显后端 message
      setSaveError(e instanceof ApiError ? e.message : '保存失败，请稍后重试');
    } finally {
      setSaving(false);
    }
  }

  // 目录选择器（计划外 Task 11）：后端调系统对话框（访达/资源管理器），
  // picked:true 回填 destDir 输入框——**不自动保存**，仍走整表单保存语义
  // （Review Focus #2）；picked:false（用户取消）静默；错误（409
  // dialog_busy / 503 fs_picker_unsupported / 网络层）toast 后端 message。
  async function handlePickDir(): Promise<void> {
    setPicking(true);
    try {
      const r = await api.post<PickDirResult>('/api/fs/pick-dir');
      if (r.picked && r.path) setDestDir(r.path);
    } catch (e) {
      toast.show(
        e instanceof ApiError ? e.message : '选择目录失败，请稍后重试',
        'error',
      );
    } finally {
      setPicking(false);
    }
  }

  if (loading) {
    return (
      <section className="settings">
        <p className="settings__busy" role="status">
          <Spinner />
          正在加载设置…
        </p>
      </section>
    );
  }
  if (loadError) {
    return (
      <section className="settings">
        <p className="settings__error" role="alert">
          {loadError}
        </p>
      </section>
    );
  }

  return (
    <section className="settings">
      <h1>设置</h1>

      <form
        className="settings__form"
        onSubmit={(e) => {
          void handleSave(e);
        }}
      >
        <Card padding="lg">
          <div className="settings__grid">
            {/* 字段级 aria-invalid 仅标注明知场景（保存失败且 destDir 为空），
                错误文案仍统一走表单级 role=alert（无字段级校验规则可复述） */}
            <Input
              label="下载目录"
              hint="下载文件的保存目录（绝对路径）"
              value={destDir}
              onChange={(e) => setDestDir(e.target.value)}
              aria-invalid={saveError !== null && destDir === ''}
            />

            {/* 系统目录选择器（计划外 Task 11）：与 Input 共存——按钮只回填
                不替代手输；type="button" 不触发表单提交；loading 复用 T9 后
                的 Button（可及名不因加载暂失）。settings.css 在本次边界外，
                按钮以独立网格行紧邻 Input（同行并排需另授权改样式）。 */}
            <div>
              <Button
                type="button"
                variant="secondary"
                loading={picking}
                onClick={() => {
                  void handlePickDir();
                }}
              >
                选择目录…
              </Button>
            </div>

            <div className="settings__field">
              <label className="settings__label" htmlFor="settings-threads">
                并发档位
              </label>
              <select
                id="settings-threads"
                className="settings__select"
                aria-describedby="settings-threads-hint"
                value={threads}
                onChange={(e) => setThreads(Number(e.target.value))}
              >
                {THREAD_STEPS.map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </select>
              <p className="settings__hint" id="settings-threads-hint">
                单个任务的连接并发数
              </p>
            </div>

            <div className="settings__field">
              <label className="settings__label" htmlFor="settings-max-tasks">
                同时任务数
              </label>
              <select
                id="settings-max-tasks"
                className="settings__select"
                aria-describedby="settings-max-tasks-hint"
                value={maxTasks}
                onChange={(e) => setMaxTasks(Number(e.target.value))}
              >
                {MAX_TASK_STEPS.map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </select>
              <p className="settings__hint" id="settings-max-tasks-hint">
                可同时下载的任务数量
              </p>
            </div>

            <fieldset className="settings__theme">
              <legend className="settings__legend">主题</legend>
              {THEME_OPTIONS.map(({ value, label }) => (
                <label
                  key={label}
                  className="settings__theme-option"
                >
                  <input
                    type="radio"
                    name="theme"
                    value={value}
                    checked={themeLock === value}
                    onChange={() => handleThemeLock(value)}
                  />
                  {label}
                </label>
              ))}
            </fieldset>

            {saveError && (
              <p className="settings__error" role="alert">
                {saveError}
              </p>
            )}

            <div className="settings__actions">
              <Button type="submit" loading={saving}>
                保存
              </Button>
            </div>
          </div>
        </Card>
      </form>
    </section>
  );
}
