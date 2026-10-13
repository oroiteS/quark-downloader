/**
 * AccountBadge（D2 左下，Task 4）：昵称 + 容量环 SVG（used/total）。
 *
 * - 容量 wire 单位为字节（Quark member 接口 int64 直通，protocol/api.go
 *   Capacity），按 1024 基准换算 GiB 显示——“G”即 GiB（简报钉死）。
 * - 环半径取 100/(2π)：周长恰为 100，dashoffset 数值即“剩余占比×100”，
 *   弧度 = (dasharray - dashoffset) / dasharray = used/total。
 * - 环为装饰（数值已由文本承载）→ aria-hidden，避免读屏双重播报（skill：
 *   装饰性 SVG 不进无障碍树）。
 */
import { useSession } from '../stores/session';

const GIB = 1024 ** 3;
/** 周长 100 的半径（viewBox 40 内含 stroke 4 恰好放下）。 */
const RADIUS = 100 / (2 * Math.PI);

function toGiB(bytes: number): number {
  return Math.round(bytes / GIB);
}

export function AccountBadge() {
  const nickname = useSession((s) => s.nickname);
  const capacity = useSession((s) => s.capacity);

  const used = capacity?.used ?? 0;
  const total = capacity?.total ?? 0;
  const ratio = total > 0 ? Math.min(used / total, 1) : 0;
  const dashoffset = 100 * (1 - ratio);

  return (
    <div className="account-badge">
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
          strokeWidth="4"
        />
        <circle
          className="capacity-ring__value"
          cx="20"
          cy="20"
          r={RADIUS}
          fill="none"
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
