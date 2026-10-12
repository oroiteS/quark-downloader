import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
// 主题跟随（D7）：applyTheme 的语义钉死——""=跟随系统（matchMedia 驱动），
// "light"/"dark"=锁定（不监听）。返回卸载函数移除监听（供后续任务/测试复用）。
import { applyTheme } from './theme';

type Listener = (e: { matches: boolean }) => void;

/** jsdom 未实现 matchMedia——注入可控 fake：记录监听器、可翻转 matches 并触发 change。 */
function fakeMatchMedia(initialMatches: boolean) {
  let matches = initialMatches;
  const listeners = new Set<Listener>();
  const mql = {
    get matches() {
      return matches;
    },
    addEventListener: (_type: 'change', cb: Listener) => {
      listeners.add(cb);
    },
    removeEventListener: (_type: 'change', cb: Listener) => {
      listeners.delete(cb);
    },
    /** 翻转系统配色并派发 change（等效真实 matchMedia 的 change 事件）。 */
    set(next: boolean) {
      matches = next;
      listeners.forEach((cb) => cb({ matches }));
    },
  };
  return mql;
}

describe('applyTheme（D7 主题跟随与锁定）', () => {
  /** install() 注册 spy 并记下恢复函数；afterEach 统一还原。 */
  let restoreSpy: () => void = () => {};

  function install(initialMatches: boolean) {
    const mql = fakeMatchMedia(initialMatches);
    const spy = vi
      .spyOn(window, 'matchMedia')
      .mockReturnValue(mql as unknown as MediaQueryList);
    restoreSpy = () => spy.mockRestore();
    return mql;
  }

  beforeEach(() => {
    document.documentElement.removeAttribute('data-theme');
  });

  afterEach(() => {
    restoreSpy();
    document.documentElement.removeAttribute('data-theme');
  });

  it('applyTheme("")：系统 dark → data-theme="dark"', () => {
    install(true);
    applyTheme('');
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
  });

  it('applyTheme("")：系统配色 change → 跟随切换', () => {
    const mql = install(false);
    applyTheme('');
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
    mql.set(true); // 系统切到 dark
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
    mql.set(false); // 再切回 light
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
  });

  it('applyTheme("light")：锁定后系统 change 不影响', () => {
    const mql = install(false);
    applyTheme('light');
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
    mql.set(true); // 系统切 dark——锁定无效
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
  });

  it('applyTheme("dark")：锁定 dark；卸载函数执行后不再监听', () => {
    install(false);
    const unapply = applyTheme('dark');
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
    void unapply; // 锁定分支的卸载函数为空操作，语义上无监听可卸

    // 对照组：跟随模式下卸载后 change 不再生效
    const mql2 = fakeMatchMedia(false);
    const spy2 = vi
      .spyOn(window, 'matchMedia')
      .mockReturnValue(mql2 as unknown as MediaQueryList);
    const un = applyTheme('');
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
    un();
    mql2.set(true);
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
    spy2.mockRestore();
  });
});
