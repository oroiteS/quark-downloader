/**
 * 扫码登录页(Task 5,D8):二维码 + 六态文案 + 终态重新生成 + Cookie 导入。
 *
 * - 轮询与终态语义全在 useQrLogin(api/auth.ts):间隔 2s,
 *   got_ticket/invalid/expired/timeout 终态停止;遇 400(终态后服务端会话
 *   已清)一并停止。页面不自行订阅会话过期跳转(T4 交接①:守卫是唯一导航
 *   权威);登录成功跳 / 属简报明示的页面业务跳转。
 * - 登录成功(扫码 got_ticket / Cookie 导入)→ GET /api/account 按 setAccount
 *   签名刷新 session store(T3 交付:setAccount 写入含 loggedIn:true)→ 跳 /。
 *   账户端点否认/失败时停留登录页(与 App.probeAccount「失败=未登录」同语义)。
 * - 多行 Cookie 输入按控制者裁决用原生 <textarea> + tokens 变量样式
 *   (T2 无 Textarea,简报词面漂移),样式落同批预豁免的 login.css。
 */
import { useEffect, useId, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { Button, Card, Spinner } from '../components/ui';
import { useQrLogin } from '../api/auth';
import { api } from '../api/client';
import { ApiError } from '../api/types';
import type { Account, QrStateValue } from '../api/types';
import { useSession } from '../stores/session';
import './login.css';

/** 六态文案(简报逐字:等待扫描/已扫描待确认/登录成功/二维码失效/已过期/超时)。 */
const STATUS_TEXT: Record<QrStateValue, string> = {
  waiting: '等待扫描',
  scanned: '已扫描待确认',
  got_ticket: '登录成功',
  invalid: '二维码失效',
  expired: '已过期',
  timeout: '超时',
};

/** 失败终态:引导重新生成(got_ticket 成功终态除外)。 */
function isFailedTerminal(state: QrStateValue | null): boolean {
  return state === 'invalid' || state === 'expired' || state === 'timeout';
}

type Navigate = ReturnType<typeof useNavigate>;

/**
 * 登录成功后续:拉 /api/account 按 setAccount 签名刷新 store,再进 /。
 * 账户端点否认或失败时置未登录并停留登录页(不发明第三种语义,用户可重试)。
 */
async function enterApp(navigate: Navigate): Promise<void> {
  try {
    const account = await api.get<Account>('/api/account');
    if (!account.logged_in) {
      useSession.getState().setLoggedIn(false);
      return;
    }
    useSession.getState().setAccount({
      nickname: account.nickname,
      avatar: account.avatar,
      capacity: account.capacity,
    });
  } catch {
    useSession.getState().setLoggedIn(false);
    return;
  }
  navigate('/');
}

export function Login() {
  const navigate = useNavigate();
  const qr = useQrLogin();
  const [cookieText, setCookieText] = useState('');
  const [cookieError, setCookieError] = useState<string | null>(null);
  const [cookieBusy, setCookieBusy] = useState(false);
  const cookieErrorId = useId();
  const enteredRef = useRef(false);

  // 挂载即发起扫码(start 内 gen 自增;卸载由 hook 清理使循环自废)。
  const startRef = qr.start;
  useEffect(() => {
    void startRef();
  }, [startRef]);

  // 扫码成功(got_ticket)→ 刷新 store 并跳 /(简报明示的页面业务跳转)。
  useEffect(() => {
    if (qr.state === 'got_ticket' && !enteredRef.current) {
      enteredRef.current = true;
      void enterApp(navigate);
    }
  }, [qr.state, navigate]);

  async function submitCookie(): Promise<void> {
    const text = cookieText.trim();
    if (!text) {
      setCookieError('请输入 Cookie 文本');
      return;
    }
    setCookieError(null);
    setCookieBusy(true);
    try {
      await api.post('/api/login/cookie', { text });
      await enterApp(navigate);
    } catch (e) {
      setCookieError(
        e instanceof ApiError ? e.message : 'Cookie 导入失败，请稍后重试',
      );
    } finally {
      setCookieBusy(false);
    }
  }

  // 状态区文案:轮询态优先;首轮轮询未归时按二维码在手与否兜底。
  const statusText = qr.state
    ? STATUS_TEXT[qr.state]
    : qr.imgSrc
      ? '等待扫描'
      : '正在生成二维码…';
  const showRegenerate = qr.error !== null || isFailedTerminal(qr.state);

  return (
    <main id="main-content" className="login">
      <Card padding="lg">
        <div className="login__card">
          <h1 className="login__title">登录</h1>

          <div className="login__qr-zone">
            {qr.imgSrc ? (
              <button
                type="button"
                className="login__qr"
                onClick={() => void qr.restart()}
              >
                <img
                  src={qr.imgSrc}
                  alt="登录二维码，点击可重新生成"
                  width={200}
                  height={200}
                />
              </button>
            ) : (
              !qr.error && (
                <div className="login__qr-fallback">
                  <Spinner />
                </div>
              )
            )}

            {qr.error ? (
              <p className="login__status login__status--error" role="alert">
                {qr.error}
              </p>
            ) : (
              <p className="login__status" aria-live="polite">
                {statusText}
              </p>
            )}

            {qr.state === 'got_ticket' && qr.nickname && (
              <p className="login__welcome">欢迎，{qr.nickname}</p>
            )}

            {showRegenerate && (
              <Button onClick={() => void qr.restart()}>重新生成二维码</Button>
            )}
          </div>

          <details className="login__cookie">
            <summary className="login__summary">导入 Cookie</summary>
            <form
              className="login__form"
              onSubmit={(e) => {
                e.preventDefault();
                void submitCookie();
              }}
            >
              <label className="login__label" htmlFor="cookie-text">
                Cookie 文本
              </label>
              <textarea
                id="cookie-text"
                className="login__textarea"
                rows={4}
                value={cookieText}
                onChange={(e) => setCookieText(e.target.value)}
                placeholder="粘贴从浏览器复制的 Cookie"
                aria-invalid={cookieError ? true : undefined}
                aria-describedby={cookieError ? cookieErrorId : undefined}
              />
              {cookieError && (
                <p id={cookieErrorId} className="login__error" role="alert">
                  {cookieError}
                </p>
              )}
              <Button type="submit" loading={cookieBusy}>
                导入并登录
              </Button>
            </form>
          </details>
        </div>
      </Card>
    </main>
  );
}
