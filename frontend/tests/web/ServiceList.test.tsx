// ServiceList：空态 / 选中态 / 设置项角标 / 回调。
import assert from 'node:assert/strict';
import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { ServiceList } from '../../src/web/components/ServiceList';
import type { ServiceCardView } from '../../src/web/types';

const services: ServiceCardView[] = [
  { name: 'images', addr: 'ws://127.0.0.1:9401/ws', description: '图片服务' },
  { name: 'qq', addr: 'ws://127.0.0.1:9301/ws' },
];

function renderList(over: Partial<Parameters<typeof ServiceList>[0]> = {}) {
  return render(
    <ServiceList services={services} selected={null} settingsCountOf={() => 0} onSelect={() => {}} {...over} />,
  );
}

describe('ServiceList', () => {
  it('空名册：显示占位', () => {
    render(<ServiceList services={[]} selected={null} settingsCountOf={() => 0} onSelect={() => {}} />);
    assert.ok(screen.getByText('尚未收到名册'));
  });

  it('选中态、设置项角标、title 回落', () => {
    renderList({ selected: 'qq', settingsCountOf: (n) => (n === 'images' ? 1 : 0) });
    const images = screen.getByText('images').closest('button');
    const qq = screen.getByText('qq').closest('button');
    assert.ok(qq?.className.includes('svc-item-active'));
    assert.ok(!images?.className.includes('svc-item-active'));
    assert.ok(screen.getByText('1')); // 角标只在有设置项的服务上
    assert.equal(images?.getAttribute('title'), '图片服务');
    assert.equal(qq?.getAttribute('title'), 'qq'); // 无简介 → 回落服务名
  });

  it('点击触发 onSelect', () => {
    const onSelect = vi.fn();
    renderList({ onSelect });
    fireEvent.click(screen.getByText('qq'));
    expect(onSelect).toHaveBeenCalledWith('qq');
  });
});
