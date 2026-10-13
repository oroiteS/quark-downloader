// 扫码登录数据层(Task 5):useQrLogin——start / 轮询 / 终态停止。
//
// 后端契约(auth.go,控制者实测钉死):
// - POST /api/login/qr/start → {token, qrcode_png(base64), scan_url};
//   重复调用 = 刷新二维码(旧会话作废)→ restart 即重发 start。
// - GET /api/login/qr/state → {state, logged_in, nickname?};六态中
//   got_ticket/invalid/expired/timeout 为终态,轮询到此即停;终态后服务端
//   会话已清,再轮询返回 400(no_qr_session)——遇 400 一并停止。
//
// 竞态防护:代际计数(genRef)——restart/卸载使旧轮询循环在下个检查点自废,
// 不出现双循环互踩;组件卸载经 effect 清理使循环必然终止。
import { useCallback, useEffect, useRef, useState } from 'react';
import { api } from './client';
import { ApiError } from './types';
import type { QrStart, QrState, QrStateValue } from './types';

/** 轮询间隔常量(简报钉死 2s)。 */
export const QR_POLL_INTERVAL_MS = 2000;

/** 终态集合:轮询到此即停(got_ticket 为成功终态,其余为失败终态)。 */
const TERMINAL_STATES: ReadonlySet<QrStateValue> = new Set([
  'got_ticket',
  'invalid',
  'expired',
  'timeout',
]);

export interface UseQrLoginResult {
  /** qrcode_png 拼好 data: 前缀,直接作 <img src>;null = 未生成/生成失败。 */
  imgSrc: string | null;
  state: QrStateValue | null;
  /** got_ticket 响应携带的昵称(成功登录后由页面接力刷新 session store)。 */
  nickname: string;
  /** start 失败或轮询错误的原因(null = 无错误)。 */
  error: string | null;
  start: () => Promise<void>;
  /** 重新 start(后端语义:旧会话作废、二维码刷新)。 */
  restart: () => Promise<void>;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

export function useQrLogin(): UseQrLoginResult {
  const [imgSrc, setImgSrc] = useState<string | null>(null);
  const [state, setState] = useState<QrStateValue | null>(null);
  const [nickname, setNickname] = useState('');
  const [error, setError] = useState<string | null>(null);
  const genRef = useRef(0);

  const start = useCallback(async (): Promise<void> => {
    const gen = ++genRef.current;
    setError(null);
    setState(null);
    setNickname('');
    setImgSrc(null);
    try {
      const res = await api.post<QrStart>('/api/login/qr/start');
      if (genRef.current !== gen) return;
      setImgSrc(`data:image/png;base64,${res.qrcode_png}`);
    } catch (e) {
      if (genRef.current !== gen) return;
      setError(e instanceof ApiError ? e.message : '二维码生成失败，请稍后重试');
      return;
    }
    // 轮询循环:间隔 2s;终态 / 400 / 其余错误即停(页面引导 restart)。
    while (genRef.current === gen) {
      await sleep(QR_POLL_INTERVAL_MS);
      if (genRef.current !== gen) return;
      try {
        const s = await api.get<QrState>('/api/login/qr/state');
        if (genRef.current !== gen) return;
        setState(s.state);
        if (s.state === 'got_ticket' && s.nickname) setNickname(s.nickname);
        if (TERMINAL_STATES.has(s.state)) return;
      } catch {
        if (genRef.current !== gen) return;
        // 400(终态后会话已清)与其余错误同处置:停止,交页面引导重新生成
        setError('二维码状态获取失败，请重新生成');
        return;
      }
    }
  }, []);

  const restart = useCallback((): Promise<void> => start(), [start]);

  // 卸载清理:活跃循环在下个检查点(gen 比对)自废。
  useEffect(() => {
    return () => {
      genRef.current += 1;
    };
  }, []);

  return { imgSrc, state, nickname, error, start, restart };
}
