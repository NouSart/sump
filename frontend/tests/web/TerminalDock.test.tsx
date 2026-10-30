// 终端：放大的输入框。开合、Esc、本地回显；本轮只有 UI，命令不执行。
import assert from 'node:assert/strict';
import { describe, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { TerminalDock } from '../../src/web/components/TerminalDock';
import type { Translate } from '../../src/web/i18n';

const tZh: Translate = (key, fallback) => fallback ?? key;

describe('TerminalDock', () => {
  it('关闭时不渲染', () => {
    const { container } = render(<TerminalDock open={false} onClose={() => {}} t={tZh} />);
    assert.equal(container.querySelector('.terminal'), null);
  });

  it('打开后显示空态与「UI 预览」提示', () => {
    render(<TerminalDock open={true} onClose={() => {}} t={tZh} />);
    assert.ok(screen.getByRole('region', { name: '终端' }));
    assert.ok(screen.getByText(/终端还没有输出/));
    assert.ok(screen.getByText('UI 预览 · 命令不会执行'));
  });

  it('输入命令只本地回显，并说明没有执行', () => {
    render(<TerminalDock open={true} onClose={() => {}} t={tZh} />);
    const input = screen.getByLabelText('终端命令') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'ls -la' } });
    // 提示符不是独立表单（外层是消息 form），回车由 input 自己处理。
    fireEvent.keyDown(input, { key: 'Enter' });
    assert.ok(screen.getByText('ls -la'));
    assert.ok(screen.getByText(/命令没有执行/));
    assert.equal(input.value, '');
  });

  it('Esc 与关闭按钮都触发 onClose', () => {
    const onClose = vi.fn();
    render(<TerminalDock open={true} onClose={onClose} t={tZh} />);
    fireEvent.keyDown(window, { key: 'Escape' });
    assert.equal(onClose.mock.calls.length, 1);

    fireEvent.click(screen.getByRole('button', { name: '关闭终端' }));
    assert.equal(onClose.mock.calls.length, 2);
  });
});
