// Card 行为钉死（简报 Interfaces）：变体与 padding 落 BEM 类名；
// interactive 渲染为原生 button 且可聚焦。
import { afterEach, describe, expect, it } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { Card } from './Card';

afterEach(cleanup);

describe('Card', () => {
  it('默认 elevated 变体渲染为 div，类名含变体与默认 padding', () => {
    const { container } = render(<Card>内容</Card>);
    const card = container.querySelector('.card')!;
    expect(card.tagName).toBe('DIV');
    expect(card).toHaveClass('card--elevated', 'card--padding-md');
  });

  it('interactive 渲染为原生 button 并可聚焦', () => {
    render(<Card interactive>整卡操作</Card>);
    const card = screen.getByRole('button', { name: '整卡操作' });
    card.focus();
    expect(document.activeElement).toBe(card);
  });

  it('variant 与 padding 变体落到类名', () => {
    const { container } = render(
      <Card variant="filled" padding="none">
        x
      </Card>,
    );
    expect(container.querySelector('.card')).toHaveClass(
      'card--filled',
      'card--padding-none',
    );
  });
});
