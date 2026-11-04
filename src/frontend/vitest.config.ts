// 组件测试（逻辑层）：jsdom + React Testing Library。
// 服务端测试仍走 node --test（tests/*.test.ts）；组件测试在 tests/web/（.test.ts(x)）。
import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'jsdom',
    include: ['tests/web/**/*.test.{ts,tsx}'],
    setupFiles: ['tests/web/setup.ts'],
    restoreMocks: true,
    unstubGlobals: true,
  },
});
