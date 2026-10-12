// 类型化 API 客户端（Task 3）：apiFetch 非 2xx → 解析 {error:{code,message}}
// 抛 ApiError（code 收窄为 29 冻结错误码联合）；非 JSON 错误体 / 未知 code →
// 兜底 internal（契约违规不崩溃）。api 快捷方法统一 JSON 收发。
import { ApiError, isErrorCode, type ErrorCode } from './types';

/**
 * apiFetch：对 /api 的 JSON 请求原语。
 *
 * - 2xx：解析并返回 JSON body（T 由调用方 DTO 类型约束）。
 * - 非 2xx：解析 {error:{code,message}} 抛 ApiError；body 非 JSON 或 code
 *   不在冻结集合 → code='internal'（status 恒保留）。
 * - 网络层异常（fetch reject）原样冒泡——本层不发明简报外的语义。
 */
export async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, init);
  if (!res.ok) {
    let code: ErrorCode = 'internal';
    let message = `请求失败（HTTP ${res.status}）`;
    try {
      const body: unknown = await res.json();
      const err = (body as { error?: { code?: unknown; message?: unknown } })
        .error;
      if (err && isErrorCode(err.code)) {
        code = err.code;
      }
      if (err && typeof err.message === 'string') {
        message = err.message;
      }
    } catch {
      // 非 JSON 错误体：保持 internal 与默认 message
    }
    throw new ApiError(code, message, res.status);
  }
  return (await res.json()) as T;
}

function jsonInit(method: 'POST' | 'PUT', body?: unknown): RequestInit {
  return {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  };
}

/** REST 快捷面（GET 无副作用直通；POST/PUT JSON 序列化收发）。 */
export const api = {
  get: <T>(path: string): Promise<T> => apiFetch<T>(path),
  post: <T>(path: string, body?: unknown): Promise<T> =>
    apiFetch<T>(path, jsonInit('POST', body)),
  put: <T>(path: string, body?: unknown): Promise<T> =>
    apiFetch<T>(path, jsonInit('PUT', body)),
};
