/**
 * AccountBadge（D2 左下，Task 4）：头像 + 昵称 + 容量环 SVG（used/total）。
 *
 * - 容量 wire 单位为字节（Quark member 接口 int64 直通，protocol/api.go
 *   Capacity），按 1024 基准换算 GiB 显示——“G”即 GiB（简报钉死）。
 * - 环半径取 100/(2π)：周长恰为 100，dashoffset 数值即“剩余占比×100”，
 *   弧度 = (dasharray - dashoffset) / dasharray = used/total。两个 circle
 *   带 stroke presentation attribute 兜底（T10 真机验收④）：CSS 类仍可
 *   覆盖（特异性高于 presentation attribute），变量/优先级任何异常下环
 *   依 token 色可见。
 * - 头像（T10 真机验收③）：avatar 有值→32px 圆形 img；无值或加载失败→
 *   昵称首字符占位圈。img alt=""（装饰——昵称已承载语义，skill：装饰性
 *   图不进无障碍树）；占位圈同理 aria-hidden。
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
  const nickname = useSession((s) => s.nickname);
  const avatar = useSession((s) => s.avatar);
  const capacity = useSession((s) => s.capacity);
  const [imgFailed, setImgFailed] = useState(false);

  // avatar 更新（重新登录/换号）时重置失败态，允许新图重试
  useEffect(() => {
    setImgFailed(false);
  }, [avatar]);

  const used = capacity?.used ?? 0;
  const total = capacity?.total ?? 0;
  const ratio = total > 0 ? Math.min(used / total, 1) : 0;
  const dashoffset = 100 * (1 - ratio);
  const showImg = avatar !== '' && !imgFailed;

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
          {(nickname || '未登录').trim().charAt(0)}
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
          stroke="var(--color-border)"
          strokeWidth="4"
        />
        <circle
          className="capacity-ring__value"
          cx="20"
          cy="20"
          r={RADIUS}
          fill="none"
          stroke="var(--color-primary)"
          strokeWidth="4"
          strokeLinecap="round"
          strokeDasharray={100}
          strokeDashoffset={dashoffset}
          transform="rotate(-90 20 20)"
        />
      </svg>
      <div className="account-badge__meta">
        <span className="account-badge__name">{nickname || '未登录'}</span>
        {capacity && (
          <span className="account-badge__capacity">
            {toGiB(used)}/{toGiB(total)}G
          </span>
        )}
      </div>
    </div>
  );
}
