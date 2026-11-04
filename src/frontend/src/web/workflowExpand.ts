import { useCallback, useEffect, useState } from 'react';

/**
 * 工作流展开：门户里"深度思考 / 终端输出"这类可折叠步骤，什么时候自动展开。
 *
 * 三档：
 *  - `collapsed` 折叠：一律收起，要看自己点（<details> 照旧可手动展开）。
 *  - `focus`     焦点展开：鼠标悬停或键盘聚焦时展开，移开就收起——默认档，
 *                过程在眼前流过但不占版面，滚过去的就过去了。
 *  - `expanded`  展开：一律展开，等于把过程全部摊开。
 *
 * 与 timeTheme 一样是**纯本地偏好**（localStorage），不走服务设置协议：
 * 它是每个人自己的阅读习惯，不需要同步给任何服务，改完立刻生效、不用重启。
 */

export type WorkflowExpand = 'collapsed' | 'focus' | 'expanded';

export const WorkflowExpandOptions: readonly WorkflowExpand[] = ['collapsed', 'focus', 'expanded'];

/** 默认档：焦点展开。 */
export const DefaultWorkflowExpand: WorkflowExpand = 'focus';

const PrefKey = 'sump.portal.workflowExpand';

export type WorkflowExpandControls = {
  mode: WorkflowExpand;
  setMode: (mode: WorkflowExpand) => void;
};

function isMode(v: unknown): v is WorkflowExpand {
  return v === 'collapsed' || v === 'focus' || v === 'expanded';
}

/** 读偏好：localStorage 里存的不认识（老版本、手改坏）就退回默认档，不抛错。 */
function readPref(): WorkflowExpand {
  try {
    const raw = window.localStorage.getItem(PrefKey);
    return isMode(raw) ? raw : DefaultWorkflowExpand;
  } catch {
    return DefaultWorkflowExpand;
  }
}

export function useWorkflowExpand(): WorkflowExpandControls {
  const [mode, setModeState] = useState<WorkflowExpand>(readPref);

  useEffect(() => {
    try {
      window.localStorage.setItem(PrefKey, mode);
    } catch {
      // 隐私模式 / 配额满：记不住就算了，不影响本次使用
    }
  }, [mode]);

  const setMode = useCallback((next: WorkflowExpand) => {
    setModeState(isMode(next) ? next : DefaultWorkflowExpand);
  }, []);

  return { mode, setMode };
}

/**
 * useTraceOpen 算一条可折叠过程条目"现在该不该展开"，并把悬停/聚焦/手动点击揉在一起：
 *
 *  - `collapsed`：始终收起；用户点开就是点开（不再自动关）。
 *  - `expanded` ：始终展开；用户点收就是收起。
 *  - `focus`    ：悬停或键盘聚焦时展开，移开/失焦收起；但用户**亲手点过**之后
 *                 就以用户的意志为准（否则刚点开就被鼠标移开抢着关掉，很恼人）。
 *
 * 用受控 open 而不是 CSS：`<details>` 的展开是浏览器自己的行为，样式表改不了 open 状态。
 */
export function useTraceOpen(mode: WorkflowExpand): {
  open: boolean;
  onToggle: (e: React.SyntheticEvent<HTMLDetailsElement>) => void;
  handlers: {
    onMouseEnter: () => void;
    onMouseLeave: () => void;
    onFocus: () => void;
    onBlur: (e: React.FocusEvent<HTMLDetailsElement>) => void;
  };
} {
  // 用户在"聚焦"状态下是否正指着/聚焦着它
  const [focused, setFocused] = useState(false);
  // 用户亲手点过之后，记下他要的状态；null = 还没表过态
  const [byUser, setByUser] = useState<boolean | null>(null);

  // 换档就把用户的手动意志清掉：换了模式还留着旧的手动状态会很鬼。
  useEffect(() => {
    setByUser(null);
    setFocused(false);
  }, [mode]);

  const open = (() => {
    if (byUser !== null) return byUser;
    if (mode === 'expanded') return true;
    if (mode === 'collapsed') return false;
    return focused;
  })();

  const onToggle = useCallback(
    (e: React.SyntheticEvent<HTMLDetailsElement>) => {
      // onToggle 在程序改 open 时也会触发（React 受控更新同样会冒这个事件），
      // 所以只在"算出来的 open"与浏览器实际状态不一致时才认定是用户动的手。
      const actual = e.currentTarget.open;
      if (actual !== open) setByUser(actual);
    },
    [open],
  );

  const handlers = {
    onMouseEnter: useCallback(() => { setFocused(true); }, []),
    onMouseLeave: useCallback(() => { setFocused(false); }, []),
    onFocus: useCallback(() => { setFocused(true); }, []),
    onBlur: useCallback((e: React.FocusEvent<HTMLDetailsElement>) => {
      // 焦点移到它内部（点文本、选字）不算失焦
      if (e.currentTarget.contains(e.relatedTarget as Node | null)) return;
      setFocused(false);
    }, []),
  };

  return { open, onToggle, handlers };
}
