// 格式化纯函数边界表（Task 7，简报 Step 1 钉死边界值表：0 速/NaN/负 ETA/
// 1024³ 边界）。词面语义：formatSpeed 自适应 B/s→MB/s 两位小数（MB 封顶）；
// formatEta <0/0/无界→"—"、≥1h→"1h 23m" 级；formatSize 统一 GiB 两位小数
// （1024 基，简报钉死单位不自适应——与 FileList 自适应版同名不同义，见报告）。
import { describe, expect, it } from 'vitest';
import { formatEta, formatSize, formatSpeed } from './format';

const KB = 1024;
const MB = 1024 ** 2;
const GB = 1024 ** 3;

describe('formatSpeed（自适应 B/s→MB/s，两位小数）', () => {
  it('0 速 → "0 B/s"', () => {
    expect(formatSpeed(0)).toBe('0 B/s');
  });
  it('NaN → "—"', () => {
    expect(formatSpeed(NaN)).toBe('—');
  });
  it('负值 → "—"', () => {
    expect(formatSpeed(-5)).toBe('—');
  });
  it('B/s 段向下取整：512.9 → "512 B/s"', () => {
    expect(formatSpeed(512.9)).toBe('512 B/s');
  });
  it('1023.9 → "1023 B/s"（不越界进 K 段）', () => {
    expect(formatSpeed(1023.9)).toBe('1023 B/s');
  });
  it('1024 → "1.00 KB/s"', () => {
    expect(formatSpeed(KB)).toBe('1.00 KB/s');
  });
  it('1536 → "1.50 KB/s"', () => {
    expect(formatSpeed(1.5 * KB)).toBe('1.50 KB/s');
  });
  it('1 MiB/s → "1.00 MB/s"', () => {
    expect(formatSpeed(MB)).toBe('1.00 MB/s');
  });
  it('1.5 MiB/s → "1.50 MB/s"', () => {
    expect(formatSpeed(1.5 * MB)).toBe('1.50 MB/s');
  });
  it('≥GB 级封顶 MB/s（简报：自适应 B/s→MB/s）', () => {
    expect(formatSpeed(3 * GB)).toBe('3072.00 MB/s');
  });
});

describe('formatEta（<0/0/无界→"—"，≥1h→"1h 23m" 级）', () => {
  it('负 ETA → "—"', () => {
    expect(formatEta(-1)).toBe('—');
  });
  it('0 → "—"（0 与负同为「无 ETA」语义）', () => {
    expect(formatEta(0)).toBe('—');
  });
  it('NaN → "—"（无界）', () => {
    expect(formatEta(NaN)).toBe('—');
  });
  it('Infinity → "—"（无界）', () => {
    expect(formatEta(Infinity)).toBe('—');
  });
  it('999ms → "<1s"', () => {
    expect(formatEta(999)).toBe('<1s');
  });
  it('59_999ms → "59s"', () => {
    expect(formatEta(59_999)).toBe('59s');
  });
  it('60_000ms → "1m"（零秒省略）', () => {
    expect(formatEta(60_000)).toBe('1m');
  });
  it('90_000ms → "1m 30s"', () => {
    expect(formatEta(90_000)).toBe('1m 30s');
  });
  it('3_599_000ms → "59m 59s"（不进位越级）', () => {
    expect(formatEta(3_599_000)).toBe('59m 59s');
  });
  it('3_600_000ms → "1h"（零分省略）', () => {
    expect(formatEta(3_600_000)).toBe('1h');
  });
  it('1h23m → "1h 23m"（简报逐字示例）', () => {
    expect(formatEta(3_600_000 + 23 * 60_000)).toBe('1h 23m');
  });
  it('小时级秒数截断：2h3m5s → "2h 3m"', () => {
    expect(formatEta(2 * 3_600_000 + 3 * 60_000 + 5_000)).toBe('2h 3m');
  });
});

describe('formatSize（GiB 两位小数，1024 基，单位统一不自适应）', () => {
  it('1024³ 边界 → "1.00 GiB"', () => {
    expect(formatSize(GB)).toBe('1.00 GiB');
  });
  it('0 → "0.00 GiB"', () => {
    expect(formatSize(0)).toBe('0.00 GiB');
  });
  it('NaN → "0.00 GiB"（兜底）', () => {
    expect(formatSize(NaN)).toBe('0.00 GiB');
  });
  it('Infinity（非有限值）兜底 "0.00 GiB"', () => {
    expect(formatSize(Infinity)).toBe('0.00 GiB');
  });
  it('1.5 GiB → "1.50 GiB"', () => {
    expect(formatSize(1.5 * GB)).toBe('1.50 GiB');
  });
  it('2.25 GiB → "2.25 GiB"', () => {
    expect(formatSize(2.25 * GB)).toBe('2.25 GiB');
  });
  it('512 B → "0.00 GiB"（小值不换单位）', () => {
    expect(formatSize(512)).toBe('0.00 GiB');
  });
});
