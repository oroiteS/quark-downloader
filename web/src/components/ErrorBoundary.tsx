/**
 * ErrorBoundary（T16 第三层顶层兜底）：React 渲染期错误在本边界捕获，
 * 渲染友好错误卡 +「重新加载」按钮，杜绝未捕获渲染错误整树卸载白屏。
 *
 * 定位（纵深防线最外圈）：页面层 files ?? [] 兜底（T16 第二层）与后端
 * nil 归一化（T16 第一层）失守时——任何 null/undefined 形状逃进渲染——
 * 由本边界接住。仅兜渲染期错误：事件回调/异步链错误不经 React 边界，
 * 由各页面 try/catch 常规处理（api 调用面均已覆盖）。
 *
 * 挂载点：App.tsx 的 <Routes> 外层（路由表内全部页面同受保护）。
 */
import { Component } from 'react';
import type { ErrorInfo, ReactNode } from 'react';
import { Button } from './ui';
import './errorboundary.css';

interface ErrorBoundaryProps {
  children: ReactNode;
}

interface ErrorBoundaryState {
  hasError: boolean;
}

export class ErrorBoundary extends Component<
  ErrorBoundaryProps,
  ErrorBoundaryState
> {
  state: ErrorBoundaryState = { hasError: false };

  static getDerivedStateFromError(): ErrorBoundaryState {
    return { hasError: true };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    // console 留原始错误现场（与页面层 catch 的 console.error 约定同型）
    console.error('渲染错误被 ErrorBoundary 捕获', error, info.componentStack);
  }

  render(): ReactNode {
    if (this.state.hasError) {
      return (
        <section className="error-boundary" role="alert">
          <h1 className="error-boundary__title">页面出错了</h1>
          <p className="error-boundary__desc">
            发生了意外错误，请重新加载页面重试。
          </p>
          <Button
            type="button"
            variant="primary"
            onClick={() => window.location.reload()}
          >
            重新加载
          </Button>
        </section>
      );
    }
    return this.props.children;
  }
}
