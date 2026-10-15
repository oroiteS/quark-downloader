/// <reference types="vitest" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// G1/G2：产物直落 gin embed 目录（internal/webapi/web/dist），后端零改动；
// 测试环境 jsdom（Vitest，主题跟随逻辑需要 documentElement）。
export default defineConfig({
  plugins: [react()],
  // dev 代理（简报 Interfaces）：本地 `pnpm dev` 时 /api 转发到 gin
  // （后端缺省监听 127.0.0.1:45678，Task 6 起双端默认对齐，dev 不再
  // 需要设 FEIWU_PORT）；build 产物同源直连，不经此代理。
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:45678',
    },
  },
  build: {
    outDir: '../internal/webapi/web/dist',
    emptyOutDir: true,
  },
  test: {
    environment: 'jsdom',
  },
})
