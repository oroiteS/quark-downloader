// Input 行为钉死（简报 Step 1）：label 经 useId 绑定；错误态 aria 三件套
// （aria-invalid + aria-describedby + role="alert"）；hint 态描述指向提示。
import { afterEach, describe, expect, it } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { Input } from './Input';

afterEach(cleanup);

describe('Input', () => {
  it('label 与输入框经 id 绑定（可按标签找到控件）', () => {
    render(<Input label="下载目录" />);
    expect(screen.getByLabelText('下载目录')).toBeInTheDocument();
  });

  it('错误态：aria-invalid 置位，aria-describedby 指向 role=alert 的错误文本', () => {
    render(<Input label="链接" error="链接无效" />);
    const input = screen.getByLabelText('链接');
    expect(input).toHaveAttribute('aria-invalid', 'true');
    const errorId = input.getAttribute('aria-describedby');
    expect(errorId).toBeTruthy();
    const errorEl = document.getElementById(errorId!);
    expect(errorEl).toHaveAttribute('role', 'alert');
    expect(errorEl).toHaveTextContent('链接无效');
  });

  it('仅 hint 时 aria-describedby 指向提示文本，aria-invalid 不置位', () => {
    render(<Input label="链接" hint="支持 http(s) 与 ed2k" />);
    const input = screen.getByLabelText('链接');
    expect(input).not.toHaveAttribute('aria-invalid', 'true');
    const hintId = input.getAttribute('aria-describedby');
    expect(hintId).toBeTruthy();
    expect(document.getElementById(hintId!)).toHaveTextContent(
      '支持 http(s) 与 ed2k',
    );
  });

  it('required 时暴露 aria-required', () => {
    render(<Input label="任务名" required />);
    // label 内含 skill 原样的 aria-label="required" * 标记，
    // label 全文为「任务名 *」，故非精确匹配定位。
    expect(screen.getByLabelText('任务名', { exact: false })).toHaveAttribute(
      'aria-required',
      'true',
    );
  });
});
