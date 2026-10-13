// API 客户端契约（Task 3，TDD 先行）：apiFetch 非 2xx → 解析 {error:{code,message}}
// 抛 ApiError（code 收窄为 31 冻结错误码联合）；非 JSON 错误体 → code='internal'。
// 错误码权威 = internal/webapi/server.go:253-283 常量块（禁从文档抄）。
import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, apiFetch } from './client';
import { ApiError } from './types';

/** 可编程 fetch 替身：按 URL 记录请求，按队列逐次应答。 */
function makeFetch(responder: (url: string, init?: RequestInit) => Response) {
  const calls: { url: string; init?: RequestInit }[] = [];
  const fn = vi.fn(async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return responder(url, init);
  });
  vi.stubGlobal('fetch', fn);
  return calls;
}

function jsonResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as unknown as Response;
}

function textResponse(status: number, text: string): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => {
      throw new SyntaxError('Unexpected token');
    },
    text: async () => text,
  } as unknown as Response;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('apiFetch（2xx 直通与错误面）', () => {
  it('2xx 返回解析后的 JSON body', async () => {
    makeFetch(() => jsonResponse(200, { hello: 'world', n: 1 }));
    await expect(apiFetch<{ hello: string }>('/api/x')).resolves.toEqual({
      hello: 'world',
      n: 1,
    });
  });

  it('400 带 {error:{code:"invalid_settings",...}} → 抛 ApiError 且 code 收窄为冻结字面量', async () => {
    makeFetch(() =>
      jsonResponse(400, {
        error: { code: 'invalid_settings', message: 'destDir 不可为空' },
      }),
    );
    const err = await apiFetch('/api/settings').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    const apiErr = err as ApiError;
    // code 类型为 ErrorCode（31 字面量联合）——invalid_settings 是其一员
    expect(apiErr.code).toBe('invalid_settings');
    expect(apiErr.status).toBe(400);
    expect(apiErr.message).toBe('destDir 不可为空');
  });

  it('非 JSON 错误体 → code="internal" 且 status 保留', async () => {
    makeFetch(() => textResponse(502, '<html>Bad Gateway</html>'));
    const err = await apiFetch('/api/links').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).code).toBe('internal');
    expect((err as ApiError).status).toBe(502);
  });

  it('错误体是 JSON 但 code 不在 31 冻结集合 → 兜底 internal（契约违规不崩溃）', async () => {
    makeFetch(() => jsonResponse(500, { error: { code: 'not_a_real_code' } }));
    const err = await apiFetch('/api/x').catch((e: unknown) => e);
    expect((err as ApiError).code).toBe('internal');
    expect((err as ApiError).status).toBe(500);
  });
});

describe('api 快捷方法（JSON 收发）', () => {
  it('get：GET 且不带 body', async () => {
    const calls = makeFetch(() => jsonResponse(200, []));
    await api.get('/api/tasks');
    expect(calls[0]?.url).toBe('/api/tasks');
    expect(calls[0]?.init?.method).toBeUndefined(); // fetch 默认 GET
    expect(calls[0]?.init?.body).toBeUndefined();
  });

  it('post：JSON 序列化 body 并带 Content-Type', async () => {
    const calls = makeFetch(() => jsonResponse(200, { sid: 's1' }));
    await api.post('/api/share/parse', { text: 'https://pan.quark.cn/s/abc' });
    expect(calls[0]?.init?.method).toBe('POST');
    expect(calls[0]?.init?.headers).toMatchObject({
      'Content-Type': 'application/json',
    });
    expect(JSON.parse(String(calls[0]?.init?.body))).toEqual({
      text: 'https://pan.quark.cn/s/abc',
    });
  });

  it('put：PUT 全量替换语义（设置页四字段齐发的载体）', async () => {
    const calls = makeFetch(() => jsonResponse(200, {}));
    await api.put('/api/settings', {
      destDir: '/tmp/d',
      threads: 32,
      maxConcurrentTasks: 3,
      themeLock: '',
    });
    expect(calls[0]?.init?.method).toBe('PUT');
    expect(JSON.parse(String(calls[0]?.init?.body))).toMatchObject({
      destDir: '/tmp/d',
      themeLock: '',
    });
  });
});
