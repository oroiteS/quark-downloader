/**
 * 扫码登录页(Task 5,D8):二维码 + 六态文案 + 终态重新生成 + Cookie 导入。
 *
 * - 轮询与终态语义全在 useQrLogin(api/auth.ts):间隔 2s,
 *   got_ticket/invalid/expired/timeout 终态停止;遇 400(终态后服务端会话
 *   已清)一并停止。页面不自行订阅会话过期跳转(T4 交接①:守卫是唯一导航
 *   权威);登录成功跳 / 属简报明示的页面业务跳转。
 * - 登录成功(扫码 got_ticket / Cookie 导入)→ GET /api/account 按 setAccount
 *   签名刷新 session store(T3 交付:setAccount 写入 loggedIn:true)→ 跳 /。
 *   账户端点否认/失败时停留登录页并给出可见错误(状态区 role=alert / Cookie
 *   面板 role=alert)+ 重新生成入口;restart 复位 enteredRef,二次扫码仍走
 *   确认链路——错误可恢复(I-1 修复),登录态一律回写 false(与
 *   App.probeAccount「失败=未登录」同语义)。
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

/** 登录确认失败文案:账户端点否认(票据/Cookie 未生效,需重新扫码或导入)。 */
const ENTER_DENIED_TEXT = '登录确认失败：账户未生效，请重新扫码或重新导入 Cookie';
/** 登录确认失败文案:账户端点请求失败(网络/服务端异常,可重试)。 */
const ENTER_ERROR_TEXT = '登录确认失败：账户信息获取失败，请重新扫码重试';

type Navigate = ReturnType<typeof useNavigate>;

/**
 * 登录成功后续:拉 /api/account 按 setAccount 签名刷新 store,再进 /。
 * 成功返回 null 并跳转;账户端点否认或失败时回写 loggedIn:false 并返回
 * 用户可见错误文案(调用方经既有 role=alert 通道展示,I-1:不再静默)。
 */
async function enterApp(navigate: Navigate): Promise<string | null> {
  try {
    const account = await api.get<Account>('/api/account');
    if (!account.logged_in) {
      useSession.getState().setLoggedIn(false);
      return ENTER_DENIED_TEXT;
    }
    useSession.getState().setAccount({
      nickname: account.nickname,
      avatar: account.avatar,
      capacity: account.capacity,
    });
  } catch {
    useSession.getState().setLoggedIn(false);
    return ENTER_ERROR_TEXT;
  }
  navigate('/');
  return null;
}

export function Login() {
  const navigate = useNavigate();
  const qr = useQrLogin();
  const [cookieText, setCookieText] = useState('');
  const [cookieError, setCookieError] = useState<string | null>(null);
  const [cookieBusy, setCookieBusy] = useState(false);
  // 登录确认(enterApp)失败的用户可见文案;restart 时复位(I-1)。
  const [enterError, setEnterError] = useState<string | null>(null);
  const cookieErrorId = useId();
  const enteredRef = useRef(false);

  // 挂载即发起扫码(start 内 gen 自增;卸载由 hook 清理使循环自废)。
  const startRef = qr.start;
  useEffect(() => {
    void startRef();
  }, [startRef]);

  // restart 统一入口:复位 enteredRef 与确认错误——失败恢复后二次扫码
  // got_ticket 仍会走确认链路(I-1:否则 enteredRef 永真,永不跳转)。
  function handleRestart(): void {
    enteredRef.current = false;
    setEnterError(null);
    void qr.restart();
  }

  // 扫码成功(got_ticket)→ 刷新 store 并跳 /(简报明示的页面业务跳转)。
  // 确认失败经 .then 入 enterError,由状态区 role=alert 展示(不再静默)。
  useEffect(() => {
    if (qr.state === 'got_ticket' && !enteredRef.current) {
      enteredRef.current = true;
      void enterApp(navigate).then(setEnterError);
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
      // enterApp 失败返回文案(I-1):经既有 Cookie 面板 role=alert 通道可见
      setCookieError(await enterApp(navigate));
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
  // 错误优先展示:qr.error=生成/轮询错误,enterError=登录确认失败(I-1)。
  const alertText = qr.error ?? enterError;
  const showRegenerate = alertText !== null || isFailedTerminal(qr.state);

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
                onClick={handleRestart}
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

            {/* 常驻单节点、按态切换 role/aria-live(审查 M-1:条件挂载漏播) */}
            <p
              className={`login__status${
                alertText ? ' login__status--error' : ''
              }`}
              role={alertText ? 'alert' : undefined}
              aria-live={alertText ? undefined : 'polite'}
            >
              {alertText ?? statusText}
            </p>

            {qr.state === 'got_ticket' && qr.nickname && !enterError && (
              <p className="login__welcome">欢迎，{qr.nickname}</p>
            )}

            {showRegenerate && (
              <Button onClick={handleRestart}>重新生成二维码</Button>
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
