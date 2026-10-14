// Toasts：两类提示的渲染与无障碍容器。
import assert from 'node:assert/strict';
import { describe, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { Toasts } from '../../src/web/components/Toasts';

describe('Toasts', () => {
  it('渲染 ok / error 两类提示（容器可无障碍播报）', () => {
    render(
      <Toasts
        items={[
          { id: 1, kind: 'ok', text: '已生效 images.max_mb = 2' },
          { id: 2, kind: 'error', text: '未生效 images.max_mb：未送达' },
        ]}
      />,
    );
    const ok = screen.getByText('已生效 images.max_mb = 2');
    const err = screen.getByText('未生效 images.max_mb：未送达');
    assert.ok(ok.className.includes('toast-ok'));
    assert.ok(err.className.includes('toast-error'));
    const box = screen.getByRole('status');
    assert.equal(box.getAttribute('aria-live'), 'polite');
  });

  it('空列表：容器在、无条目', () => {
    const { container } = render(<Toasts items={[]} />);
    assert.equal(container.querySelectorAll('.toast').length, 0);
  });
});
