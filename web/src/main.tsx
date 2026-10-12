import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import './theme.css';
import { applyTheme } from './theme';

// D7：启动即按系统配色挂主题；设置页锁定后由 applyTheme(lock) 重挂（Task 9）。
applyTheme('');

// Task 1 占位壳：App/路由在 Task 4 落地。
createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <div className="app-bootstrap">飞舞</div>
  </StrictMode>,
);
