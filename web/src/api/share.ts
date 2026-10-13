// 分享/任务数据层（Task 6）。后端契约（share.go / tasks.go，
// 控制者实测钉死，已与盘上源码逐字核对）：
// - POST /api/share/parse {text} → 200 正常 {sid, pwd_id, passcode, files[]}；
//   纯口令场景 200 {kouling:true, hint}（不是错误）；400 invalid_share_text。
// - GET /api/share/files?sid=&pdir_fid= → {files: FileEntry[]}；sid 失效 →
//   404 no_share_session（后端文案含「请重新解析」）。
// - POST /api/tasks {source:'share', fids[], sid, dest} →
//   {task_ids[], skipped:[{fid,name,reason}]}（dest=相对全局 destDir 的子目录
//   前缀，空串=全局根；目录勾选=整棵子树交后端展开）。
import { api } from './client';
import type { FileEntry, ParseResult } from './types';

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
