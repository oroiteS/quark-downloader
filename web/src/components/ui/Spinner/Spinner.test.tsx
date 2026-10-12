// Spinner 行为钉死：渲染 aria-hidden 装饰性指示器——
// 加载语义由宿主承担（如 Button 的 aria-busy），不向读屏器单独播报。
import { afterEach, describe, expect, it } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { cleanup, render } from '@testing-library/react';
import { Spinner } from './Spinner';

afterEach(cleanup);

describe('Spinner', () => {
  it('渲染 .spinner 且 aria-hidden（装饰性，不产生读屏噪音）', () => {
    const { container } = render(<Spinner />);
    const spinner = container.querySelector('.spinner');
    expect(spinner).not.toBeNull();
    expect(spinner).toHaveAttribute('aria-hidden', 'true');
  });
});
