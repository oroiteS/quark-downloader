/**
 * 主题应用（D7）：飞舞双主题的唯一挂载点。
 *
 * - lock=""（跟随系统）：按 matchMedia('prefers-color-scheme: dark') 现值
 *   设置 data-theme，并监听 change 实时跟随（系统切换即时生效）。
 * - lock="light"/"dark"（设置页锁定）：写死，不监听。
 * - 返回卸载函数：移除 change 监听（测试与后续任务重挂时使用）。
 */
export type ThemeLock = '' | 'light' | 'dark';

const QUERY = '(prefers-color-scheme: dark)';

export function applyTheme(lock: ThemeLock): () => void {
  const root = document.documentElement;
  const media = window.matchMedia(QUERY);

  const apply = () => {
    root.setAttribute('data-theme', media.matches ? 'dark' : 'light');
  };

  if (lock) {
    root.setAttribute('data-theme', lock);
    return () => {};
  }

  apply();
  media.addEventListener('change', apply);
  return () => {
    media.removeEventListener('change', apply);
  };
}
