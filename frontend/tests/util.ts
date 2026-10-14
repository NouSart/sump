// 测试工具：等待条件、睡一会儿。
export function waitFor(cond: () => boolean, timeoutMS = 3000, what = '条件'): Promise<void> {
  const deadline = Date.now() + timeoutMS;
  return new Promise<void>((resolve, reject) => {
    const tick = (): void => {
      if (cond()) {
        resolve();
        return;
      }
      if (Date.now() > deadline) {
        reject(new Error(`等待超时: ${what}`));
        return;
      }
      setTimeout(tick, 20);
    };
    tick();
  });
}

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
