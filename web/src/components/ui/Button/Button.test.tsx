// Button 行为钉死（简报 Step 1）：loading 态渲染 Spinner 且 aria-busy；
// 禁用/加载中不触发 onClick；变体与尺寸落 BEM 类名。
import { afterEach, describe, expect, it, vi } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { Button } from './Button';

afterEach(cleanup);

describe('Button', () => {
  it('loading 态渲染 Spinner 并置 aria-busy', () => {
    render(<Button loading>保存</Button>);
    const button = screen.getByRole('button');
    expect(button).toHaveAttribute('aria-busy', 'true');
    expect(button.querySelector('.spinner')).not.toBeNull();
  });

  it('disabled 时不触发 onClick', () => {
    const onClick = vi.fn();
    render(
      <Button disabled onClick={onClick}>
        删除
      </Button>,
    );
    fireEvent.click(screen.getByRole('button', { name: '删除' }));
    expect(onClick).not.toHaveBeenCalled();
  });

  it('loading 时不触发 onClick', () => {
    const onClick = vi.fn();
    render(
      <Button loading onClick={onClick}>
        保存
      </Button>,
    );
    fireEvent.click(screen.getByRole('button'));
    expect(onClick).not.toHaveBeenCalled();
  });

  it('变体与尺寸落到 BEM 类名', () => {
    render(
      <Button variant="secondary" size="lg">
        次级
      </Button>,
    );
    expect(screen.getByRole('button', { name: '次级' })).toHaveClass(
      'btn--secondary',
      'btn--lg',
    );
  });
});
