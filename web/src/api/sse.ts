// SSE 数据层（Task 3）：GET /api/events 的 EventSource 单例与帧分发。
//
// 契约（internal/webapi/events.go 实测 wire）：帧 {type, taskId?, payload?}
// ——驼峰 taskId（events.go:22 json tag；plan 文档的 task_id 是笔误，以代码
// 为准）。四型：
// - hello：payload=任务数组（与 GET /api/tasks 同形）→ tasks.replaceAll
//   整体覆盖（断线对账：EventSource 自动重连后服务端订阅即发 hello，
//   覆盖即对账——Review Focus #1）。
// - task_progress / task_final：按 taskId 定向更新。
// - auth：分发 session store；expired → loggedIn=false + navigate('/login')
//   （Focus #3，pendingParseText 由 logoutReset 保留）。
//
// 任务 store 本体归任务列表任务（Task 5+）：本层以 TaskStoreHooks 适配器
// 注入，行为语义（覆盖 vs 更新）在本层测试钉死，接线方只需实现三方法。
import type {
  AuthPayload,
  FinalPayload,
  ProgressPayload,
  SseFrame,
  Task,
} from './types';
import { useSession } from '../stores/session';

/** 任务视图适配器：hello=整体替换，progress/final=按 id 定向更新。 */
export interface TaskStoreHooks {
  replaceAll: (tasks: Task[]) => void;
  applyProgress: (taskId: string, progress: ProgressPayload) => void;
  applyFinal: (taskId: string, final: FinalPayload) => void;
}

export interface ConnectEventsOptions {
  /** auth expired 的导航出口（路由器 history.push 等）。 */
  navigate?: (path: string) => void;
  /** 任务视图适配器（省略则 hello/进度/终态帧仅更新 session，不碰任务）。 */
  tasks?: TaskStoreHooks;
}

/** EventSource 单例：null=未连接。 */
let activeSource: EventSource | null = null;

/**
 * connectEvents：建立 SSE 连接（幂等——已连接时直接返回 no-op 关闭函数，
 * 不重复建连）。断线由 EventSource 自动重连（本层不 close 不重建）；
 * 返回的关闭函数供登出/卸载显式断开，之后可重新 connect。
 */
export function connectEvents(options: ConnectEventsOptions = {}): () => void {
  if (activeSource) {
    return () => {};
  }
  const source = new EventSource('/api/events');
  activeSource = source;
  source.onmessage = (ev: MessageEvent<string>) => {
    dispatchFrame(options, ev.data);
  };
  return () => {
    source.close();
    if (activeSource === source) {
      activeSource = null;
    }
  };
}

/** 单帧分发：坏 JSON 静默丢弃（心跳注释行不会进 onmessage；畸形 data 属契约违规）。 */
function dispatchFrame(options: ConnectEventsOptions, raw: string): void {
  let frame: SseFrame;
  try {
    frame = JSON.parse(raw) as SseFrame;
  } catch {
    return;
  }
  switch (frame.type) {
    case 'hello': {
      const tasks = Array.isArray(frame.payload)
        ? (frame.payload as Task[])
        : [];
      options.tasks?.replaceAll(tasks);
      break;
    }
    case 'task_progress': {
      if (frame.taskId && frame.payload) {
        options.tasks?.applyProgress(
          frame.taskId,
          frame.payload as ProgressPayload,
        );
      }
      break;
    }
    case 'task_final': {
      if (frame.taskId && frame.payload) {
        options.tasks?.applyFinal(frame.taskId, frame.payload as FinalPayload);
      }
      break;
    }
    case 'auth': {
      if (frame.payload) {
        handleAuth(frame.payload as AuthPayload, options.navigate);
      }
      break;
    }
  }
}

/** auth 帧分发：login 同步登录态；logout/expired 走登出重置；expired 另跳登录页。 */
function handleAuth(payload: AuthPayload, navigate?: (path: string) => void): void {
  const session = useSession.getState();
  switch (payload.event) {
    case 'login':
      session.setLoggedIn(payload.logged_in);
      break;
    case 'logout':
      session.logoutReset();
      break;
    case 'expired':
      session.logoutReset();
      navigate?.('/login');
      break;
  }
}
