/**
 * 装饰性加载指示器（aria-hidden）：加载语义由宿主承担
 * （如 Button 的 aria-busy），避免读屏器对同一状态双重播报。
 */
export function Spinner() {
  return <span className="spinner" aria-hidden="true" />;
}
