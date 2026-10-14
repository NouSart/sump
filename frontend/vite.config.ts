import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// 面板前端：构建到 dist/web（由服务端静态托管）；开发时把 /api 代理到本地服务。
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist/web',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:9501',
    },
  },
});
