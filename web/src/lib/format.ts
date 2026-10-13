/**
 * 速度/ETA/大小格式化纯函数（Task 7，简报 Produces 钉死）。
 *
 * - formatSpeed：自适应 B/s→MB/s 两位小数（MB 封顶——简报钉死上限）；
 *   非有限值/负值→"—"。
 * - formatEta：<0/0/无界（NaN/Infinity）→"—"（0 与负同为「无 ETA」语义）；
 *   <1s→"<1s"；秒/分级零值省略（"1m""1m 30s"）；≥1h→"1h 23m" 级（秒截断）。
 * - formatSize：统一 GiB 两位小数（1024 基，单位不自适应——简报钉死）；
 *   非有限值/非正值兜底 "0.00 GiB"。
 *
 * 注意：FileList 曾有自己的 formatSize（自适应 B→TB 版，Task 6），与本模块
 * 同名不同义（Task 7 报告疑虑披露过）；Task 8 起已统一改引本模块的
 * formatSize（见 FileList.tsx 头注），同名不同义不复存在，全站唯一实现。
 */

export function formatSpeed(bps: number): string {
  if (!Number.isFinite(bps) || bps < 0) return '—';
  if (bps < 1024) return `${Math.floor(bps)} B/s`;
  if (bps < 1024 ** 2) return `${(bps / 1024).toFixed(2)} KB/s`;
  return `${(bps / 1024 ** 2).toFixed(2)} MB/s`;
}

export function formatEta(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '—';
  if (ms < 1000) return '<1s';
  if (ms < 60_000) return `${Math.floor(ms / 1000)}s`;
  if (ms < 3_600_000) {
    const m = Math.floor(ms / 60_000);
    const s = Math.floor((ms % 60_000) / 1000);
    return s === 0 ? `${m}m` : `${m}m ${s}s`;
  }
  const h = Math.floor(ms / 3_600_000);
  const m = Math.floor((ms % 3_600_000) / 60_000);
  return m === 0 ? `${h}h` : `${h}h ${m}m`;
}

export function formatSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0.00 GiB';
  return `${(bytes / 1024 ** 3).toFixed(2)} GiB`;
}
