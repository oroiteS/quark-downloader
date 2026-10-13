/**
 * 任务页（Task 7，/tasks 路由）：SSE 驱动的任务列表 + 批次分组（G11）。
 *
 * 数据源 = 任务 store（SSE hello 对账 / progress/final 定向更新，三方法
 * 本体在 stores/tasks.ts，main.tsx 注入 connectEvents）。挂载时无条件
 * GET /api/tasks 一次性拉取兜底：SSE 未连接时它是唯一数据源（简报「降级
 * 拉取」）；已连接时 hello 为同源快照、立即覆盖对账，两者数据等价——
 * SSE 连接状态不在本任务可感知面（connectEvents 未暴露），此为边界内实现
 * （决策已记录报告）。列表按 created_at 精确值分组：组头「批次 HH:MM ·
 * N 项」，新批次在前、组内新任务在前。
 */
import { useEffect } from 'react';
import { api } from '../api/client';
import type { Task } from '../api/types';
import { TaskRow } from '../components/TaskRow';
import { toast } from '../components/ui';
import { useTasks, type TaskView } from '../stores/tasks';
import './tasks.css';

/** created_at → 「HH:MM」（本地时区，补零）。 */
function hhmm(ts: number): string {
  const d = new Date(ts);
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}

export interface TaskBatch {
  createdAt: number;
  label: string;
  items: TaskView[];
}

/** created_at 精确值分组（G11）：批次降序，组内按 created_at 降序。 */
export function groupByBatch(tasks: TaskView[]): TaskBatch[] {
  const buckets = new Map<number, TaskView[]>();
  for (const t of [...tasks].sort((a, b) => b.created_at - a.created_at)) {
    const bucket = buckets.get(t.created_at);
    if (bucket) bucket.push(t);
    else buckets.set(t.created_at, [t]);
  }
  return [...buckets.entries()]
    .sort((a, b) => b[0] - a[0])
    .map(([createdAt, items]) => ({
      createdAt,
      label: `批次 ${hhmm(createdAt)} · ${items.length} 项`,
      items,
    }));
}

export function Tasks() {
  const tasks = useTasks((s) => s.tasks);

  // SSE 未连接降级：挂载一次性拉取（裸数组 taskDTO）；失败弹错不阻塞——
  // hello 到达后仍会对账补齐。
  useEffect(() => {
    let cancelled = false;
    api
      .get<Task[]>('/api/tasks')
      .then((list) => {
        if (!cancelled) {
          useTasks.getState().replaceAll(Array.isArray(list) ? list : []);
        }
      })
      .catch(() => {
        if (!cancelled) toast.show('任务列表加载失败', 'error');
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const batches = groupByBatch(tasks);

  return (
    <section className="tasks">
      <h1>任务</h1>
      {batches.length === 0 ? (
        <p className="tasks__empty" role="status">
          暂无下载任务
        </p>
      ) : (
        batches.map((batch) => (
          <section key={batch.createdAt} className="tasks__batch">
            <h2 className="tasks__batch-head">{batch.label}</h2>
            <ul className="tasks__list">
              {batch.items.map((t) => (
                <TaskRow key={t.id} task={t} />
              ))}
            </ul>
          </section>
        ))
      )}
    </section>
  );
}
