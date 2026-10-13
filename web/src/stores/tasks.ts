/**
 * 任务 store（Task 7，zustand）：TaskStoreHooks（sse.ts:25-30）三方法本体。
 *
 * 数据权威 = SSE：hello → replaceAll 整体替换（断线对账）；progress/final
 * → 按 id 定向更新。merging 只在 progress 帧出现（GET 快照无此字段）——
 * applyProgress 写入；applyFinal 清为 false（终态即合并完成）；replaceAll
 * 直收 wire Task（快照无 merging，自然不残留）。未知 id 的定向更新静默
 * 忽略（SSE 帧先于快照到达的乱序防御，随后 hello 会补齐视图）。
 *
 * 操作面（pause/resume/cancel/retry POST）不在 store：组件直调 api，
 * 操作后 UI 以 store/SSE 为权威（简报钉死，本 store 不发对账请求）。
 */
import { create } from 'zustand';
import type { FinalPayload, ProgressPayload, Task } from '../api/types';

/** 视图条目：wire Task + merging（SSE progress 帧专属，GET 快照无）。 */
export type TaskView = Task & { merging?: boolean };

interface TasksState {
  tasks: TaskView[];
  replaceAll: (tasks: Task[]) => void;
  applyProgress: (taskId: string, progress: ProgressPayload) => void;
  applyFinal: (taskId: string, final: FinalPayload) => void;
}

export const useTasks = create<TasksState>()((set) => ({
  tasks: [],
  replaceAll: (tasks) => set({ tasks: [...tasks] }),
  applyProgress: (taskId, p) =>
    set((s) => ({
      tasks: s.tasks.map((t) =>
        t.id === taskId
          ? {
              ...t,
              done: p.done,
              size: p.total,
              speed_bps: p.speed_bps,
              eta_ms: p.eta_ms,
              state: p.state,
              merging: p.merging,
            }
          : t,
      ),
    })),
  applyFinal: (taskId, f) =>
    set((s) => ({
      tasks: s.tasks.map((t) =>
        t.id === taskId
          ? { ...t, state: f.state, error: f.error, merging: false }
          : t,
      ),
    })),
}));

/**
 * connectEvents 注入适配器（main.tsx 接线点）：三方法与 TaskStoreHooks
 * 签名逐字一致，经 getState() 转发（引用稳定，不随渲染重建）。
 */
export const taskStoreHooks = {
  replaceAll: (tasks: Task[]): void => useTasks.getState().replaceAll(tasks),
  applyProgress: (taskId: string, progress: ProgressPayload): void =>
    useTasks.getState().applyProgress(taskId, progress),
  applyFinal: (taskId: string, final: FinalPayload): void =>
    useTasks.getState().applyFinal(taskId, final),
};
