// SettingsSheet 是设置抽屉：右侧滑出，左侧服务导航 + 右侧服务设置面板。
//
// 为什么是抽屉而不是一个"页"：对话才是主体，设置是低频操作——不该把对话整页顶掉。
// 主题开关也收进这里（草图里侧栏只有 SUMP / 会话 / 用户 / 收起）。
import { useEffect, type ReactNode } from 'react';
import { IconDark, IconLight } from './icons';

interface Props {
  theme: 'light' | 'dark';
  onTheme: (t: 'light' | 'dark') => void;
  onClose: () => void;
  children: ReactNode;
}

export function SettingsSheet({ theme, onTheme, onClose, children }: Props) {
  // Esc 关闭：抽屉是浮层，必须有键盘出口。
  useEffect(() => {
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('keydown', onKey);
    };
  }, [onClose]);

  return (
    <div
      className="scrim"
      role="presentation"
      onMouseDown={(e) => {
        // 只有点在遮罩本身（不是抽屉内部）才关
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <section className="sheet" role="dialog" aria-modal="true" aria-label="设置">
        <header className="sheet-head">
          <h1 className="sheet-title">设置</h1>
          <span className="grow" />
          <div className="theme-switch" role="radiogroup" aria-label="外观">
            <button
              type="button"
              role="radio"
              aria-checked={theme === 'light'}
              className="theme-tab"
              onClick={() => {
                onTheme('light');
              }}
            >
              <IconLight />
              浅色
            </button>
            <button
              type="button"
              role="radio"
              aria-checked={theme === 'dark'}
              className="theme-tab"
              onClick={() => {
                onTheme('dark');
              }}
            >
              <IconDark />
              深色
            </button>
          </div>
          <button type="button" className="chip" onClick={onClose} aria-label="关闭设置">
            关闭
          </button>
        </header>
        <div className="sheet-body">{children}</div>
      </section>
    </div>
  );
}
