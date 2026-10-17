// Markdown：把模型的回复当 Markdown 渲染（代码块 / 列表 / 表格 / 链接）。
//
// 用 react-markdown 而不是"转 HTML 再塞 dangerouslySetInnerHTML"：模型输出不可信，
// 默认不解析原始 HTML，天然挡掉注入。单换行用 remark-breaks 保留（模型的回复常靠换行分段）。
import type { ComponentProps, ReactElement } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkBreaks from 'remark-breaks';
import remarkGfm from 'remark-gfm';

function Link({ href, children, ...rest }: ComponentProps<'a'>): ReactElement {
  return (
    <a {...rest} href={href} target="_blank" rel="noreferrer noopener">
      {children}
    </a>
  );
}

export function Markdown({ text }: { text: string }): ReactElement {
  return (
    <div className="md">
      <ReactMarkdown remarkPlugins={[remarkGfm, remarkBreaks]} components={{ a: Link }}>
        {text}
      </ReactMarkdown>
    </div>
  );
}
