// 分享/任务/直链数据层（Task 6）。后端契约（share.go / tasks.go / links.go，
// 控制者实测钉死，已与盘上源码逐字核对）：
// - POST /api/share/parse {text} → 200 正常 {sid, pwd_id, passcode, files[]}；
//   纯口令场景 200 {kouling:true, hint}（不是错误）；400 invalid_share_text。
// - GET /api/share/files?sid=&pdir_fid= → {files: FileEntry[]}；sid 失效 →
//   404 no_share_session（后端文案含「请重新解析」）。
// - POST /api/tasks {source:'share', fids[], sid, dest} →
//   {task_ids[], skipped:[{fid,name,reason}]}（dest=相对全局 destDir 的子目录
//   前缀，空串=全局根；目录勾选=整棵子树交后端展开）。
// - POST /api/links {source:'share', fids[], sid} →
//   {items:[{file_name,size,url,score}], expires_hint}（条目无 fid/rel_path
//   字段；fix round 1 W1：目录项由前端过滤不上送——后端对目录 fid 无处理
//   必报错；W2：导出 rel_path 由前端按勾选来源相对路径组装——根=file_name，
//   子目录=面包屑相对路径/file_name，同名按勾选序对位）。
// - POST /api/links/export {items:[{rel_path,url}], format} → 200 纯文本
//   （c.String，非 JSON！）。T3 账本交接：apiFetch 2xx 硬 res.json() 对文本
//   响应会崩——exportLinksText 走原始 fetch + res.text() 专用通道；非 2xx
//   错误面与 apiFetch 语义一致（就近复刻，不改 client.ts 本体）。
import { api } from './client';
import { ApiError, isErrorCode } from './types';
import type { ErrorCode, FileEntry, LinksResult, ParseResult } from './types';

/** 纯口令解析结果（share.go：口令场景 200，显示 hint 不报错）。 */
export interface KoulingParse {
  kouling: true;
  hint: string;
}

export type ShareParseResult = ParseResult | KoulingParse;

/** 口令场景判别（响应只在口令场景带 kouling 字段）。 */
export function isKoulingParse(r: ShareParseResult): r is KoulingParse {
  return (r as KoulingParse).kouling === true;
}

/** 解析分享文本（粘贴框全文直传，后端识别链接/口令）。 */
export function parseShareText(text: string): Promise<ShareParseResult> {
  return api.post<ShareParseResult>('/api/share/parse', { text });
}

/** 目录导航：pdir_fid 空串 = 根列表（面包屑返回根同走此端点）。 */
export function listShareFiles(
  sid: string,
  pdirFid = '',
): Promise<{ files: FileEntry[] }> {
  return api.get<{ files: FileEntry[] }>(
    `/api/share/files?sid=${encodeURIComponent(sid)}&pdir_fid=${encodeURIComponent(pdirFid)}`,
  );
}

/** 建任务响应：task_ids + skipped（逐条 toast 反馈的来源）。 */
export interface CreateTasksResult {
  task_ids: string[];
  skipped: Array<{ fid: string; name: string; reason: string }>;
}

/** 提交下载（勾选 fids 原样上送：目录项由后端整棵子树展开）。 */
export function createShareTasks(
  sid: string,
  fids: readonly string[],
  dest: string,
): Promise<CreateTasksResult> {
  return api.post<CreateTasksResult>('/api/tasks', {
    source: 'share',
    fids: [...fids],
    sid,
    dest,
  });
}

/** 批量取直链（W1：目录项由调用方过滤后再上送；时效文案在 expires_hint）。 */
export function getShareLinks(
  sid: string,
  fids: readonly string[],
): Promise<LinksResult> {
  return api.post<LinksResult>('/api/links', {
    source: 'share',
    fids: [...fids],
    sid,
  });
}

/** 导出五格式（export.go 枚举，顺序即默认 tab 序）。 */
export type ExportFormat = 'plain' | 'named' | 'aria2' | 'curl' | 'motrix';

/** 导出请求单条（W2：rel_path 由调用方按勾选路径上下文组装——根=file_name，
 *  子目录=面包屑相对路径/file_name，named 等格式保层级、重名不冲突）。 */
export interface LinkExportItem {
  rel_path: string;
  url: string;
}

/**
 * 导出为文本（纯文本专用通道，见文件头交接说明）。
 * 非 2xx 与 apiFetch 同语义：解析 {error:{code,message}} 抛 ApiError，
 * 非 JSON 错误体兜底 internal。
 */
export async function exportLinksText(
  items: readonly LinkExportItem[],
  format: ExportFormat,
): Promise<string> {
  const res = await fetch('/api/links/export', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ items: [...items], format }),
  });
  if (!res.ok) {
    let code: ErrorCode = 'internal';
    let message = `请求失败（HTTP ${res.status}）`;
    try {
      const body = (await res.json()) as {
        error?: { code?: unknown; message?: unknown };
      };
      const err = body.error;
      if (err && isErrorCode(err.code)) code = err.code;
      if (err && typeof err.message === 'string') message = err.message;
    } catch {
      // 非 JSON 错误体：保持 internal 与默认 message
    }
    throw new ApiError(code, message, res.status);
  }
  // 2xx 纯文本：res.text() 专用通道（绝不 res.json()）
  return res.text();
}
