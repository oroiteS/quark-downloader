/**
 * 任务行（Task 7）：文件名 + rel_dir（等宽小字 --font-mono）+ 进度条
 * （role="progressbar" + aria-valuenow/max）+ 速度/ETA/大小 + state 徽章
 * + 操作集（frontend-designer SKILL 为规范权威：原生语义优先、全 tokens 变量）。
 *
 * 操作集（简报逐字）：running→暂停/取消；paused→继续/取消；终态重试仅
 * error/canceled；needs_reparse→文案逐字「需重新解析分享」可点跳解析页
 * （/），不提供 retry；exists→徽章「已存在」无操作；merging=true→进度条
 * 置灰显示「合并中」；error→错误文本 + 「复制错误信息」（剪贴板）。
 *
 * 操作 POST 后 UI 以 store/SSE 为权威（简报钉死）：成功不发对账请求、
 * 不本地改状态；失败弹错误 toast。retry 响应 {task_id} 为复活新 ID，
 * 新条目由 hello/后续帧入 store——本组件不消费响应体。
 */
import { useNavigate } from 'react-router-dom';
import { api } from '../api/client';
import type { TaskState } from '../api/types';
import { formatEta, formatSize, formatSpeed } from '../lib/format';
import type { TaskView } from '../stores/tasks';
import { Button, toast } from './ui';
import './task-row.css';

/** 九词面 → 徽章文案与色调（未知词面兜底显示原值，渲染不崩溃）。 */
const STATE_BADGE: Partial<Record<TaskState, { text: string; tone: string }>> = {
  pending: { text: '等待中', tone: 'neutral' },
  running: { text: '下载中', tone: 'info' },
  paused: { text: '已暂停', tone: 'warning' },
  done: { text: '已完成', tone: 'success' },
  finished: { text: '已完成', tone: 'success' },
  error: { text: '失败', tone: 'error' },
  canceled: { text: '已取消', tone: 'neutral' },
  needs_reparse: { text: '需重新解析', tone: 'warning' },
  exists: { text: '已存在', tone: 'info' },
};

export function TaskRow({ task }: { task: TaskView }) {
  const navigate = useNavigate();
  const badge = STATE_BADGE[task.state];
  const isMerging = task.merging === true;
  const pct =
    task.size > 0 ? Math.min(100, Math.round((task.done / task.size) * 100)) : 0;

  /** 操作 POST：失败弹错（成功静默——store/SSE 为权威）。 */
  async function act(action: 'pause' | 'resume' | 'cancel' | 'retry'): Promise<void> {
    try {
      await api.post(`/api/tasks/${task.id}/${action}`);
    } catch {
      toast.show('操作失败，请重试', 'error');
    }
  }

  async function copyError(): Promise<void> {
    try {
      await navigator.clipboard.writeText(task.error ?? '');
    } catch {
      toast.show('复制失败', 'error');
    }
  }

  return (
    <li className="task-row">
      <div className="task-row__head">
        <span className="task-row__name">{task.file_name}</span>
        {badge ? (
          <span className={`task-badge task-badge--${badge.tone}`}>{badge.text}</span>
        ) : (
          <span className="task-badge">{task.state}</span>
        )}
      </div>

      {task.rel_dir ? <p className="task-row__dir">{task.rel_dir}</p> : null}

      <div
        className="task-row__bar"
        role="progressbar"
        aria-label={`${task.file_name} 下载进度`}
        aria-valuemin={0}
        aria-valuenow={task.done}
        aria-valuemax={task.size}
      >
        <span
          className={`task-row__bar-fill${isMerging ? ' task-row__bar-fill--merging' : ''}`}
          style={{ width: `${pct}%` }}
        />
      </div>

      <div className="task-row__meta">
        {isMerging ? (
          <span className="task-row__merging">合并中</span>
        ) : (
          <>
            <span>{formatSpeed(task.speed_bps)}</span>
            <span>{formatEta(task.eta_ms)}</span>
          </>
        )}
        <span>{formatSize(task.size)}</span>
      </div>

      {task.state === 'error' && task.error ? (
        <p className="task-row__error">
          <span className="task-row__error-text">{task.error}</span>
          <Button variant="ghost" size="sm" onClick={() => void copyError()}>
            复制错误信息
          </Button>
        </p>
      ) : null}

      <div className="task-row__actions">
        {task.state === 'running' ? (
          <>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => void act('pause')}
            >
              暂停
            </Button>
            <Button variant="ghost" size="sm" onClick={() => void act('cancel')}>
              取消
            </Button>
          </>
        ) : null}
        {task.state === 'paused' ? (
          <>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => void act('resume')}
            >
              继续
            </Button>
            <Button variant="ghost" size="sm" onClick={() => void act('cancel')}>
              取消
            </Button>
          </>
        ) : null}
        {task.state === 'error' || task.state === 'canceled' ? (
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void act('retry')}
          >
            重试
          </Button>
        ) : null}
        {task.state === 'needs_reparse' ? (
          <Button variant="secondary" size="sm" onClick={() => navigate('/')}>
            需重新解析分享
          </Button>
        ) : null}
      </div>
    </li>
  );
}
