// 任务 store（Task 7）行为钉死：sse.ts:25-30 TaskStoreHooks 三方法契约的
// 本体实现——hello=整体替换（断线对账）、progress=按 id 定向更新并写入
// merging、final=终态+错误并清 merging。语义基准=sse.test.ts 替身。
import { beforeEach, describe, expect, it } from 'vitest';
import { useTasks } from './tasks';
import type { ProgressPayload, Task } from '../api/types';

function makeTask(id: string, state: Task['state'] = 'running'): Task {
  return {
    id,
    file_name: `${id}.zip`,
    rel_dir: '',
    size: 100,
    state,
    done: 0,
    speed_bps: 0,
    eta_ms: 0,
    mode: 'a',
    created_at: 1700000000000,
  };
}

const PROGRESS: ProgressPayload = {
  done: 50,
  total: 200,
  speed_bps: 2048,
  eta_ms: 25_000,
  state: 'running',
  merging: true,
};

beforeEach(() => {
  useTasks.getState().replaceAll([]);
});

describe('useTasks（TaskStoreHooks 三方法本体）', () => {
  it('replaceAll 整体覆盖（SSE hello 断线对账语义，无幽灵残留）', () => {
    useTasks.getState().replaceAll([makeTask('t1'), makeTask('t2')]);
    expect(useTasks.getState().tasks.map((t) => t.id)).toEqual(['t1', 't2']);
    useTasks.getState().replaceAll([makeTask('t2')]);
    expect(useTasks.getState().tasks.map((t) => t.id)).toEqual(['t2']);
  });

  it('applyProgress 按 id 更新进度字段并写入 merging', () => {
    useTasks.getState().replaceAll([makeTask('t1'), makeTask('t2')]);
    useTasks.getState().applyProgress('t1', PROGRESS);
    const [t1, t2] = useTasks.getState().tasks;
    expect(t1).toMatchObject({
      done: 50,
      size: 200,
      speed_bps: 2048,
      eta_ms: 25_000,
      state: 'running',
      merging: true,
    });
    expect(t2).toMatchObject({ done: 0, size: 100 });
    expect(t2.merging).toBeUndefined(); // 未被 progress 触碰,无 merging 键
  });

  it('applyProgress 未知 id 不新增条目（SSE 先于快照的乱序防御）', () => {
    useTasks.getState().applyProgress('ghost', PROGRESS);
    expect(useTasks.getState().tasks).toEqual([]);
  });

  it('applyFinal 写终态与错误文本并清 merging（终态即合并完成）', () => {
    useTasks.getState().replaceAll([makeTask('t1')]);
    useTasks.getState().applyProgress('t1', PROGRESS);
    useTasks.getState().applyFinal('t1', { state: 'done' });
    const t1 = useTasks.getState().tasks[0]!;
    expect(t1.state).toBe('done');
    expect(t1.error).toBeUndefined();
    expect(t1.merging).toBe(false);
  });

  it('applyFinal 携带错误文本时写入 error', () => {
    useTasks.getState().replaceAll([makeTask('t1')]);
    useTasks.getState().applyFinal('t1', { state: 'error', error: '磁盘已满' });
    const t1 = useTasks.getState().tasks[0]!;
    expect(t1.state).toBe('error');
    expect(t1.error).toBe('磁盘已满');
  });

  it('replaceAll 覆盖清 merging（GET 快照无 merging 字段，不残留）', () => {
    useTasks.getState().replaceAll([makeTask('t1')]);
    useTasks.getState().applyProgress('t1', PROGRESS);
    useTasks.getState().replaceAll([makeTask('t1')]);
    expect(useTasks.getState().tasks[0]!.merging).toBeUndefined();
  });
});
