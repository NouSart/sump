import assert from 'node:assert/strict';
import test from 'node:test';
import { parseTimerItems } from '../src/server/service.ts';

const silent = (): void => {};

test('parseTimerItems 非数组返回空', () => {
  assert.deepEqual(parseTimerItems(undefined, silent), []);
  assert.deepEqual(parseTimerItems({}, silent), []);
});

test('parseTimerItems 合法项保留，单字段坏的项丢弃', () => {
  const good = { item_id: 'i1', caller: 'c', ms: 100, fire_at: '2026-01-01T00:00:00Z', note: 'n' };
  const out = parseTimerItems([good], silent);
  assert.equal(out.length, 1);
  assert.equal(out[0]?.itemId, 'i1');

  for (const bad of [
    { item_id: '', ms: 100, fire_at: 'x' },
    { item_id: 'i2', ms: '100', fire_at: 'x' },
    { item_id: 'i3', ms: 100, fire_at: 42 },
  ]) {
    assert.deepEqual(parseTimerItems([bad], silent), []);
  }
});
