import type { ButtonHTMLAttributes, ReactNode } from 'react';
import { Spinner } from '../Spinner/Spinner';
import './button.css';

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: 'primary' | 'secondary' | 'ghost';
  size?: 'sm' | 'md' | 'lg';
  /**
   * 加载中：渲染 Spinner、置 aria-busy 并一并禁用
   * （skill Button 模式：disabled || loading 同源，杜绝竞态点击）。
   */
  loading?: boolean;
  children?: ReactNode;
}

/** 页面唯一按钮原语：BEM 变体命名，样式全 token 化（skill Button 模式）。 */
export function Button({
  variant = 'primary',
  size = 'md',
  loading = false,
  disabled = false,
  children,
  onClick,
  ...props
}: ButtonProps) {
  return (
    <button
      className={`btn btn--${variant} btn--${size}`}
      disabled={disabled || loading}
      aria-busy={loading}
      onClick={onClick}
      {...props}
    >
      {loading ? <Spinner /> : children}
    </button>
  );
}
