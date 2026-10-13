/**
 * AccountBadge（D2 左下，Task 4）：头像 + 昵称 + 容量环 SVG（used/total）。
 *
 * - 容量 wire 单位为字节（Quark member 接口 int64 直通，protocol/api.go
 *   Capacity），按 1024 基准换算 GiB 显示——“G”即 GiB（简报钉死）。
 * - 环半径取 100/(2π)：周长恰为 100，dashoffset 数值即“剩余占比×100”，
 *   弧度 = (dasharray - dashoffset) / dasharray = used/total。两个 circle
 *   的 stroke 兜底走 inline style（T10 真机验收④，round 2 审查 I-1 修正）：
 *   var() 在 SVG presentation attribute 中不生效（引擎未实现，解析失败
 *   回落 none，兜底形同虚设），inline style 的 var() 合法有效；inline
 *   优先级高于类规则（app.css）正是兜底语义——类规则被浏览器插件改写/
 *   失效时，inline 仍按 token 变量渲染，主题切换依旧跟随变量。
 * - 头像（T10 真机验收③）：avatar 有值→32px 圆形 img；无值或加载失败→
 *   昵称首字符占位圈。img alt=""（装饰——昵称已承载语义，skill：装饰性
 *   图不进无障碍树）；占位圈同理 aria-hidden。
 * - 昵称占位（T15 登录态与昵称解耦）：登录态下后端回退链也拿不到昵称时
 *   （200 {logged_in:true, nickname:""}）显示「夸克用户」占位——「未登录」
 *   字样仅 loggedIn=false 出现，昵称空不再暗示未登录。
 * - 环为装饰（数值已由文本承载）→ aria-hidden，避免读屏双重播报（skill：
 *   装饰性 SVG 不进无障碍树）。
 */
import { useEffect, useState } from 'react';
import { useSession } from '../stores/session';

const GIB = 1024 ** 3;
/** 周长 100 的半径（viewBox 40 内含 stroke 4 恰好放下）。 */
const RADIUS = 100 / (2 * Math.PI);

function toGiB(bytes: number): number {
  return Math.round(bytes / GIB);
}

export function AccountBadge() {
  const loggedIn = useSession((s) => s.loggedIn);
  const nickname = useSession((s) => s.nickname);
  const avatar = useSession((s) => s.avatar);
  const capacity = useSession((s) => s.capacity);
  const [imgFailed, setImgFailed] = useState(false);

  // 昵称占位（T15）：空昵称按登录态分流——已登录「夸克用户」、未登录（含
  // 探测中 loggedIn=null，壳层尚不渲染本组件，纯防御）保持「未登录」。
  const displayName = nickname || (loggedIn ? '夸克用户' : '未登录');

  // avatar 更新（重新登录/换号）时重置失败态，允许新图重试
  useEffect(() => {
    setImgFailed(false);
  }, [avatar]);

  const used = capacity?.used ?? 0;
  const total = capacity?.total ?? 0;
  const ratio = total > 0 ? Math.min(used / total, 1) : 0;
  const dashoffset = 100 * (1 - ratio);
  const showImg = avatar !== '' && !imgFailed;

  // 容量环三级色（用户需求，T10 真机复验二轮纳入）：占用 <70% 蓝
  // （primary）、70%（含）～90% 黄（warning）、90%（含）以上红（error，
  // ratio 已钳 1——可超 100% 显示满环红）。环 aria-hidden 装饰，色对
  // surface 的对比度按 WCAG 图形 3:1 阈值自查（tokens.css 头注释）。
  const ringColor =
    ratio < 0.7
      ? 'var(--color-primary)'
      : ratio < 0.9
        ? 'var(--color-warning)'
        : 'var(--color-error)';

  return (
    <div className="account-badge">
      {showImg ? (
        <img
          className="account-badge__avatar"
          src={avatar}
          alt=""
          width={32}
          height={32}
          onError={() => setImgFailed(true)}
        />
      ) : (
        <span
          className="account-badge__avatar account-badge__avatar--fallback"
          aria-hidden="true"
        >
          {displayName.trim().charAt(0)}
        </span>
      )}
      <svg
        className="capacity-ring"
        viewBox="0 0 40 40"
        width={40}
        height={40}
        aria-hidden="true"
        focusable="false"
      >
        <circle
          className="capacity-ring__track"
          cx="20"
          cy="20"
          r={RADIUS}
          fill="none"
          style={{ stroke: 'var(--color-border)' }}
          strokeWidth="4"
        />
        <circle
          className="capacity-ring__value"
          cx="20"
          cy="20"
          r={RADIUS}
          fill="none"
          style={{ stroke: ringColor }}
          strokeWidth="4"
          strokeLinecap="round"
          strokeDasharray={100}
          strokeDashoffset={dashoffset}
          transform="rotate(-90 20 20)"
        />
      </svg>
      <div className="account-badge__meta">
        <span className="account-badge__name">{displayName}</span>
        {capacity && (
          <span className="account-badge__capacity">
            {toGiB(used)}/{toGiB(total)}G
          </span>
        )}
      </div>
    </div>
  );
}
