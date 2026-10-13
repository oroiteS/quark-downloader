// wire 契约类型（Task 3）：与后端实测形态逐字对齐。
//
// 权威来源（禁从文档抄，以代码为准）：
// - 错误码 29 项冻结：internal/webapi/server.go:253-283 常量块
// - SSE 帧型：internal/webapi/events.go:21 Event{type, taskId?, payload?}
//   （json tag 为驼峰 taskId——plan 文档 Global Constraints 的 task_id 是笔误）
// - taskDTO：internal/webapi/tasks.go:500-512
// - FileEntry：internal/protocol/api.go:22-28
// - Settings：internal/store/settings.go:15-18
// - auth 载荷：internal/webapi/events.go:127 authPayload

// ---- 错误面 ----

/** 29 项冻结错误码（server.go 常量块逐字；增删须同步后端护栏测试）。 */
export type ErrorCode =
  | 'not_logged_in'
  | 'internal'
  | 'not_found'
  | 'task_not_found'
  | 'needs_reparse'
  | 'dest_busy'
  | 'submit_failed'
  | 'invalid_threads'
  | 'manager_closed'
  | 'invalid_tasks_request'
  | 'save_tasks_failed'
  | 'save_cookie_failed'
  | 'logout_failed'
  | 'exchange_failed'
  | 'qr_start_failed'
  | 'qr_poll_failed'
  | 'no_qr_session'
  | 'invalid_cookie'
  | 'share_token_failed'
  | 'links_failed'
  | 'expand_failed'
  | 'capacity_failed'
  | 'invalid_share_text'
  | 'share_list_failed'
  | 'drive_list_failed'
  | 'no_share_session'
  | 'invalid_links_request'
  | 'invalid_settings'
  | 'save_settings_failed';

/** 后端错误响应统一形态：{error:{code,message}}。 */
export interface ApiErrorBody {
  error: { code: string; message: string };
}

const FROZEN_ERROR_CODES: readonly ErrorCode[] = [
  'not_logged_in',
  'internal',
  'not_found',
  'task_not_found',
  'needs_reparse',
  'dest_busy',
  'submit_failed',
  'invalid_threads',
  'manager_closed',
  'invalid_tasks_request',
  'save_tasks_failed',
  'save_cookie_failed',
  'logout_failed',
  'exchange_failed',
  'qr_start_failed',
  'qr_poll_failed',
  'no_qr_session',
  'invalid_cookie',
  'share_token_failed',
  'links_failed',
  'expand_failed',
  'capacity_failed',
  'invalid_share_text',
  'share_list_failed',
  'drive_list_failed',
  'no_share_session',
  'invalid_links_request',
  'invalid_settings',
  'save_settings_failed',
];

/** 运行时守卫：未知 code（契约违规）不进类型面，调用方兜底 internal。 */
export function isErrorCode(v: unknown): v is ErrorCode {
  return (
    typeof v === 'string' &&
    (FROZEN_ERROR_CODES as readonly string[]).includes(v)
  );
}

/** 类型化 API 错误：code 收窄为 29 字面量联合，status 保留 HTTP 语义。 */
export class ApiError extends Error {
  readonly code: ErrorCode;
  readonly status: number;

  constructor(code: ErrorCode, message: string, status: number) {
    super(message);
    this.name = 'ApiError';
    this.code = code;
    this.status = status;
  }
}

// ---- DTO ----

/**
 * 任务状态九词面（Ruling T3-F1 控制者钉死）：pending|running|paused|done|
 * error|canceled|finished|needs_reparse|exists。终态失败=error、取消=canceled
 * （T3 曾按 deact 旧词面写 failed/cancelled，Task 7 起以本词面为准）。
 * 闭合集合（不再留 (string & {}) 兜底）：渲染层对未知值走默认分支兜底，不崩溃。
 */
export type TaskState =
  | 'pending'
  | 'running'
  | 'paused'
  | 'done'
  | 'error'
  | 'canceled'
  | 'finished'
  | 'needs_reparse'
  | 'exists';

/** taskDTO（tasks.go:500-512 逐字；GET /api/tasks 响应与 SSE hello 载荷同形）。 */
export interface Task {
  id: string;
  file_name: string;
  rel_dir: string;
  size: number;
  state: TaskState;
  done: number;
  speed_bps: number;
  eta_ms: number;
  mode: string;
  error?: string;
  needs_reparse?: boolean;
  created_at: number;
}

/** 文件条目（protocol/api.go FileEntry；列表端点响应 {files:[FileEntry]}）。 */
export interface FileEntry {
  fid: string;
  file_name: string;
  size: number;
  dir: boolean;
  share_fid_token: string;
}

/** 设置（store/settings.go json tag；PUT 为全量替换语义——四字段必须齐发）。 */
export interface Settings {
  destDir: string;
  threads: number;
  maxConcurrentTasks: number;
  themeLock: '' | 'light' | 'dark';
}

/** 网盘容量（auth.go account 端点 capacity 字段）。 */
export interface Capacity {
  total: number;
  used: number;
}

/** 账户信息（auth.go handleAccount 响应）。 */
export interface Account {
  logged_in: boolean;
  nickname: string;
  avatar: string;
  capacity: Capacity;
}

/** 扫码登录发起（auth.go handleQRStart 响应；qrcode_png 为 base64）。 */
export interface QrStart {
  token: string;
  qrcode_png: string;
  scan_url: string;
}

/** 扫码状态六态（auth.go qrStateName 映射表；got_ticket 时带 nickname）。 */
export type QrStateValue =
  | 'waiting'
  | 'scanned'
  | 'got_ticket'
  | 'invalid'
  | 'expired'
  | 'timeout';

export interface QrState {
  state: QrStateValue;
  logged_in: boolean;
  nickname?: string;
}

/** 分享解析结果（share.go handleShareParse 响应；stoken 由后端会话持有绝不透出）。 */
export interface ParseResult {
  sid: string;
  pwd_id: string;
  passcode: string;
  files: FileEntry[];
}

/** 直链条目（links.go handleLinks items 元素）。 */
export interface LinkItem {
  file_name: string;
  size: number;
  url: string;
  score: number;
}

/** 批量取直链响应（links.go；expires_hint 为时效提示文案）。 */
export interface LinksResult {
  items: LinkItem[];
  expires_hint: string;
}

// ---- SSE 帧型（events.go Event/progressPayload/finalPayload/authPayload）----

export type SseEventType = 'hello' | 'task_progress' | 'task_final' | 'auth';

/** SSE 帧 wire 形态：{type, taskId?, payload?}——驼峰 taskId（events.go:22 json tag）。 */
export interface SseFrame {
  type: SseEventType;
  taskId?: string;
  payload?: unknown;
}

/** task_progress 载荷（字段与 taskDTO 同形子集）。 */
export interface ProgressPayload {
  done: number;
  total: number;
  speed_bps: number;
  eta_ms: number;
  state: TaskState;
  merging: boolean;
}

/** task_final 载荷：终态 + 错误文本（失败详情唯一来源）。 */
export interface FinalPayload {
  state: TaskState;
  error?: string;
}

export type AuthEvent = 'login' | 'logout' | 'expired';

/** auth 载荷：登录态三事件。 */
export interface AuthPayload {
  event: AuthEvent;
  logged_in: boolean;
}
