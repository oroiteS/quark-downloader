import type { ReactNode } from 'react';
import './card.css';

export interface CardProps {
  variant?: 'elevated' | 'outlined' | 'filled';
  padding?: 'none' | 'sm' | 'md' | 'lg';
  /**
   * 整卡可点：渲染为原生 button——语义、焦点、禁用全原生自带，
   * 不再重复 role="button"（skill 首则：原生语义优先于 ARIA 复述）。
   */
  interactive?: boolean;
  children: ReactNode;
}

/** 卡片容器原语：BEM 变体命名，样式全 token 化（skill Card 模式）。 */
export function Card({
  variant = 'elevated',
  padding = 'md',
  interactive = false,
  children,
}: CardProps) {
  const className = `card card--${variant} card--padding-${padding}${
    interactive ? ' card--interactive' : ''
  }`;

  if (interactive) {
    return (
      <button type="button" className={className}>
        {children}
      </button>
    );
  }

  return <div className={className}>{children}</div>;
}
