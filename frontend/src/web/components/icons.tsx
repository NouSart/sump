import type { ReactElement } from 'react';

// 图标集：与 SUMP 字标同一套几何取向——16 网格、细线（1.25）、圆角端点与拐角、直白无装饰的形状、
// 只用最少笔画（密度小）。颜色一律 currentColor；按钮自己负责 aria-label 与 title。
const box = {
  width: 20,
  height: 20,
  viewBox: '0 0 16 16',
  fill: 'none',
  xmlns: 'http://www.w3.org/2000/svg',
  'aria-hidden': true,
  stroke: 'currentColor',
  strokeWidth: 1.25,
  strokeLinecap: 'round',
  strokeLinejoin: 'round',
} as const;

export function IconSliders(): ReactElement {
  return (
    <svg {...box}>
      <path d="M2.5 5.2h2.3M8.9 5.2h4.6M2.5 10.8h5.6M12.2 10.8h1.3" />
      <circle cx="7.2" cy="5.2" r="1.4" />
      <circle cx="10.5" cy="10.8" r="1.4" />
    </svg>
  );
}

export function IconPaperclip(): ReactElement {
  return (
    <svg {...box}>
      <path d="M12 5.2 6.6 10.6a2 2 0 0 1-2.8-2.8l5.2-5.2a1.5 1.5 0 0 1 2.1 2.1l-5 5a.9.9 0 0 1-1.3-1.3l4.6-4.6" />
    </svg>
  );
}

export function IconTerminal(): ReactElement {
  return (
    <svg {...box}>
      <path d="M4.2 5.4 6.8 8l-2.6 2.6" />
      <path d="M8.6 10.9h3.3" />
    </svg>
  );
}

export function IconSend(): ReactElement {
  return (
    <svg {...box}>
      <path d="M8 12.6V3.6" />
      <path d="M4.3 7.3 8 3.6l3.7 3.7" />
    </svg>
  );
}

export function IconSun(): ReactElement {
  return (
    <svg {...box}>
      <circle cx="8" cy="8" r="2.5" />
      <path d="M8 2.1v1.2M8 12.7v1.2M2.1 8h1.2M12.7 8h1.2M3.85 3.85l.85.85M11.3 11.3l.85.85M12.15 3.85l-.85.85M4.7 11.3l-.85.85" />
    </svg>
  );
}

export function IconMoon(): ReactElement {
  return (
    <svg {...box}>
      <path d="M12.8 9.5A5.3 5.3 0 0 1 6.5 3.2a5.3 5.3 0 1 0 6.3 6.3Z" />
    </svg>
  );
}

export function IconClose(): ReactElement {
  return (
    <svg {...box}>
      <path d="M4.4 4.4 11.6 11.6M11.6 4.4 4.4 11.6" />
    </svg>
  );
}

export function IconCheck(): ReactElement {
  return (
    <svg {...box}>
      <path d="M3.6 8.4 6.5 11.3 12.4 4.8" />
    </svg>
  );
}

export function IconPencil(): ReactElement {
  return (
    <svg {...box}>
      <path d="M4.2 11.8 4.8 9.1 10.6 3.3a1.3 1.3 0 0 1 1.9 0l.3.3a1.3 1.3 0 0 1 0 1.9L7 11.2Z" />
    </svg>
  );
}

export function IconUndo(): ReactElement {
  return (
    <svg {...box}>
      <path d="M3.2 7.2A4.8 4.8 0 1 0 6.1 3" />
      <path d="M3.2 2.6v4.6h4.6" />
    </svg>
  );
}

export function IconChevronUp(): ReactElement {
  return (
    <svg {...box}>
      <path d="M4.2 9.8 8 6l3.8 3.8" />
    </svg>
  );
}

export function IconChevronDown(): ReactElement {
  return (
    <svg {...box}>
      <path d="M4.2 6.2 8 10l3.8-3.8" />
    </svg>
  );
}