// SSE 数据层契约（Task 3，TDD 先行）：connectEvents 的帧分发与断线对账。
// Review Focus #1 钉死：重连后首个 hello 必须整体覆盖本地任务视图。
// wire 帧型以 internal/webapi/events.go 实测为准：{type, taskId?, payload?}（驼峰 taskId）。
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { connectEvents, type TaskStoreHooks } from './sse';
import { useSession } from '../stores/session';
import type { FinalPayload, ProgressPayload, Task } from './types';

/** jsdom 未实现 EventSource——可控 mock：记录实例，测试直接派发帧。 */
class MockEventSource {
  static instances: MockEventSource[] = [];
  url: string;
  onmessage: ((ev: { data: string }) => void) | null = null;
  onerror: ((ev: unknown) => void) | null = null;
  onopen: (() => void) | null = null;
  closed = false;
  constructor(url: string) {
    this.url = url;
    MockEventSource.instances.push(this);
  }
  close() {
    this.closed = true;
  }
  /** 派发一帧 SSE data（与浏览器一致的 JSON 文本形态）。 */
  emit(frame: unknown) {
    this.onmessage?.({ data: JSON.stringify(frame) });
  }
  /** 模拟连接断开（浏览器随后自动重连，同一 EventSource 对象继续收帧）。 */
  drop() {
    this.onerror?.({ type: 'error' });
  }
}

/** 内存任务 store 替身：任务 store 本体归后续任务（Task 5+），此处实现
 * 「hello=整体覆盖、progress/final=按 id 更新」的目标语义供断言。 */
function makeMemoryTaskStore() {
  let tasks: Task[] = [];
  const hooks: TaskStoreHooks = {
    replaceAll(next) {
      tasks = [...next];
    },
    applyProgress(id, p: ProgressPayload) {
      tasks = tasks.map((t) =>
        t.id === id
          ? { ...t, done: p.done, size: p.total, speed_bps: p.speed_bps, eta_ms: p.eta_ms, state: p.state }
          : t,
      );
    },
    applyFinal(id, f: FinalPayload) {
      tasks = tasks.map((t) =>
        t.id === id ? { ...t, state: f.state, error: f.error } : t,
      );
    },
  };
  return { hooks, list: () => tasks };
}

function makeTask(id: string, state = 'running'): Task {
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

let store: ReturnType<typeof makeMemoryTaskStore>;
let navigate: ReturnType<typeof vi.fn>;
let closeEvents: (() => void) | null = null;

beforeEach(() => {
  MockEventSource.instances = [];
  vi.stubGlobal('EventSource', MockEventSource);
  store = makeMemoryTaskStore();
  navigate = vi.fn();
  useSession.setState({
    loggedIn: null,
    nickname: '',
    avatar: '',
    capacity: null,
    pendingParseText: '',
  });
});

afterEach(() => {
  closeEvents?.();
  closeEvents = null;
  vi.unstubAllGlobals();
});

describe('connectEvents（EventSource 单例与帧分发）', () => {
  it('建立 /api/events 连接且二次调用不重复建连（单例）', () => {
    closeEvents = connectEvents({ navigate, tasks: store.hooks }); // 真关闭句柄存 afterEach 用
    connectEvents({ navigate, tasks: store.hooks }); // 已连接 → 幂等 no-op
    expect(MockEventSource.instances).toHaveLength(1);
    expect(MockEventSource.instances[0]?.url).toBe('/api/events');
  });

  it('hello payload 整体覆盖任务 store（2 任务）', () => {
    closeEvents = connectEvents({ navigate, tasks: store.hooks });
    const es = MockEventSource.instances[0]!;
    es.emit({ type: 'hello', payload: [makeTask('t1'), makeTask('t2')] });
    expect(store.list().map((t) => t.id)).toEqual(['t1', 't2']);
  });

  it('task_progress 按 taskId 更新进度字段', () => {
    closeEvents = connectEvents({ navigate, tasks: store.hooks });
    const es = MockEventSource.instances[0]!;
    es.emit({ type: 'hello', payload: [makeTask('t1'), makeTask('t2')] });
    es.emit({
      type: 'task_progress',
      taskId: 't1',
      payload: { done: 50, total: 100, speed_bps: 2048, eta_ms: 25000, state: 'running', merging: false },
    });
    const t1 = store.list().find((t) => t.id === 't1');
    expect(t1?.done).toBe(50);
    expect(t1?.size).toBe(100);
    expect(t1?.speed_bps).toBe(2048);
    expect(t1?.eta_ms).toBe(25000);
    expect(t1?.state).toBe('running');
  });

  it('task_final 按 taskId 写终态与错误文本', () => {
    closeEvents = connectEvents({ navigate, tasks: store.hooks });
    const es = MockEventSource.instances[0]!;
    es.emit({ type: 'hello', payload: [makeTask('t1')] });
    es.emit({
      type: 'task_final',
      taskId: 't1',
      payload: { state: 'failed', error: '磁盘已满' },
    });
    const t1 = store.list().find((t) => t.id === 't1');
    expect(t1?.state).toBe('failed');
    expect(t1?.error).toBe('磁盘已满');
  });

  it('断开→重连→hello(1 任务)：store 整体对账为恰好该 1 任务，无幽灵残留', () => {
    closeEvents = connectEvents({ navigate, tasks: store.hooks });
    const es = MockEventSource.instances[0]!;
    es.emit({ type: 'hello', payload: [makeTask('t1'), makeTask('t2')] });
    es.emit({
      type: 'task_progress',
      taskId: 't1',
      payload: { done: 50, total: 100, speed_bps: 1, eta_ms: 1, state: 'running', merging: false },
    });
    // 断线：sse.ts 不主动 close（EventSource 自动重连语义），同一对象继续收帧
    es.drop();
    expect(es.closed).toBe(false);
    // 重连成功后服务端首个 hello（此刻只剩 t2）
    es.emit({ type: 'hello', payload: [makeTask('t2')] });
    const ids = store.list().map((t) => t.id);
    expect(ids).toEqual(['t2']);
  });

  it('auth expired → 调用导航回调跳 /login 且 loggedIn=false；解析页文本保留（Focus #3）', () => {
    useSession.setState({ pendingParseText: 'https://pan.quark.cn/s/xyz' });
    closeEvents = connectEvents({ navigate, tasks: store.hooks });
    const es = MockEventSource.instances[0]!;
    es.emit({ type: 'auth', payload: { event: 'expired', logged_in: false } });
    expect(navigate).toHaveBeenCalledWith('/login');
    expect(useSession.getState().loggedIn).toBe(false);
    expect(useSession.getState().pendingParseText).toBe('https://pan.quark.cn/s/xyz');
  });

  it('auth login → loggedIn 同步；logout → 登出重置', () => {
    closeEvents = connectEvents({ navigate, tasks: store.hooks });
    const es = MockEventSource.instances[0]!;
    es.emit({ type: 'auth', payload: { event: 'login', logged_in: true } });
    expect(useSession.getState().loggedIn).toBe(true);
    es.emit({ type: 'auth', payload: { event: 'logout', logged_in: false } });
    expect(useSession.getState().loggedIn).toBe(false);
    expect(useSession.getState().nickname).toBe('');
  });

  it('关闭函数关连接并允许重连（新实例）', () => {
    const close = connectEvents({ navigate, tasks: store.hooks });
    const first = MockEventSource.instances[0]!;
    close();
    expect(first.closed).toBe(true);
    const close2 = connectEvents({ navigate, tasks: store.hooks });
    close2();
    expect(MockEventSource.instances).toHaveLength(2);
  });

  it('非 JSON 坏帧静默丢弃不崩溃', () => {
    closeEvents = connectEvents({ navigate, tasks: store.hooks });
    const es = MockEventSource.instances[0]!;
    expect(() => es.onmessage?.({ data: 'not-json{{' })).not.toThrow();
    expect(store.list()).toEqual([]);
  });
});
