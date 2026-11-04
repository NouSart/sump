// 组件测试的全局清理：卸载 React 树（避免用例间泄漏）；全局替身的还原由
// vitest 的 unstubGlobals 负责。
import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/react';

afterEach(() => {
  cleanup();
});
