import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// 面板前端：构建到 dist/web（由服务端静态托管）；开发时把 /api 代理到本地服务。
// changeOrigin 是必须的：门户的信任栅栏钉 Host，开发服务器（5173）原样转发会被当成外站域名拒绝。
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist/web',
    emptyOutDir: true,
  },
  server: {
    host: '127.0.0.1',
    port: 5173,
    proxy: {
      '/api': { target: 'http://127.0.0.1:9501', changeOrigin: true },
    },
  },
});
