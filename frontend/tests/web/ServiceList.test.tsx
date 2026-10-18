// ServiceList：空态 / 中文短名 + 服务名 / 选中态 / 角标 / 回调。
import assert from 'node:assert/strict';
import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { ServiceList } from '../../src/web/components/ServiceList';
import type { Translate } from '../../src/web/i18n';
import type { ServiceCardView } from '../../src/web/types';

const tZh: Translate = (key, fallback) => fallback ?? key;

const services: ServiceCardView[] = [
  { name: 'images', title: '图片', addr: 'ws://127.0.0.1:9401/ws', description: '图片服务' },
  { name: 'qq', addr: 'ws://127.0.0.1:9301/ws' },
];

function renderList(over: Partial<Parameters<typeof ServiceList>[0]> = {}) {
  return render(
    <ServiceList
      services={services}
      selected={null}
      settingsCountOf={() => 0}
      onSelect={() => {}}
      t={tZh}
      {...over}
    />,
  );
}

describe('ServiceList', () => {
  it('空名册：显示占位', () => {
    render(
      <ServiceList services={[]} selected={null} settingsCountOf={() => 0} onSelect={() => {}} t={tZh} />,
    );
    assert.ok(screen.getByText('还没有收到任何服务的设置声明'));
  });

  it('导航说人话：中文短名在前，服务名退成灰字；没短名就只显示服务名', () => {
    renderList();
    assert.ok(screen.getByText('图片')); // 服务自报的 title
    assert.ok(screen.getByText('images')); // 技术名仍在（灰字）
    assert.ok(screen.getByText('qq')); // 没 title → 只有服务名
  });

  it('选中态、角标、title 回落', () => {
    renderList({ selected: 'qq', settingsCountOf: (n) => (n === 'images' ? 1 : 0) });
    const images = screen.getByText('图片').closest('button');
    const qq = screen.getByText('qq').closest('button');
    assert.ok(qq?.className.includes('svc-item-active'));
    assert.ok(!images?.className.includes('svc-item-active'));
    assert.ok(screen.getByText('1'));
    assert.equal(images?.getAttribute('title'), '图片服务');
    assert.equal(qq?.getAttribute('title'), 'qq');
  });

  it('点击触发 onSelect', () => {
    const onSelect = vi.fn();
    renderList({ onSelect });
    fireEvent.click(screen.getByText('qq'));
    expect(onSelect).toHaveBeenCalledWith('qq');
  });
});