import React, { useMemo } from 'react';
import ReactMarkdown from 'react-markdown';
import type { Components, Options } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import remarkMath from 'remark-math';
import rehypeRaw from 'rehype-raw';
import rehypeSanitize from 'rehype-sanitize';
import rehypeKatex from 'rehype-katex';
import rehypeHighlight from 'rehype-highlight';
import 'katex/dist/katex.min.css';
import './markdown.css';
import { CodeBlock } from './CodeBlock';
import { HtmlPreview } from './HtmlPreview';
import { codeOf, fenceRawDocuments, normalizeMath, previewKind, sanitizeSchema } from './utils';

type PluggableList = NonNullable<Options['rehypePlugins']>;

const remarkPlugins: PluggableList = [remarkGfm, remarkMath];

// 顺序有讲究：先把裸 HTML 解析进语法树、再清洗，之后才插入可信的公式与高亮标记
// （公式渲染结果带 style 属性，放在清洗之前会被剥掉）。
const baseRehype: PluggableList = [
  rehypeRaw,
  [rehypeSanitize, sanitizeSchema],
  [rehypeKatex, { throwOnError: false, strict: 'ignore' }],
];
const fullRehype: PluggableList = [...baseRehype, [rehypeHighlight, { detect: false }]];

function buildComponents(streaming: boolean): Components {
  return {
    pre: ({ node, children }) => {
      const { lang, code } = codeOf(node as never);
      const kind = previewKind(lang, code);
      // 生成中的 HTML 先看源码：边收边渲染会让 iframe 每个增量都重载闪烁
      if (kind && !streaming && code) {
        return <HtmlPreview code={code} kind={kind} source={children} lang={lang} />;
      }
      return (
        <CodeBlock lang={lang} code={code} streaming={streaming && kind !== null}>
          {children}
        </CodeBlock>
      );
    },
    a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noopener noreferrer" />,
    table: ({ node: _node, ...props }) => (
      <div className="md-table">
        <table {...props} />
      </div>
    ),
    img: ({ node: _node, ...props }) => <img {...props} loading="lazy" />,
  };
}

const streamingComponents = buildComponents(true);
const finalComponents = buildComponents(false);

interface MarkdownViewProps {
  content: string;
  /** 流式生成中：跳过语法高亮（长代码每个增量都重新高亮太重），HTML 暂不预览 */
  streaming?: boolean;
  className?: string;
}

/** 助手消息渲染：GFM、公式、代码高亮、HTML/SVG 沙箱预览（不带围栏的整份文档也算），裸 HTML 经白名单清洗 */
export const MarkdownView: React.FC<MarkdownViewProps> = React.memo(({ content, streaming = false, className }) => {
  const md = useMemo(() => normalizeMath(fenceRawDocuments(content)), [content]);
  return (
    <div className={className ? `md-body ${className}` : 'md-body'}>
      <ReactMarkdown
        remarkPlugins={remarkPlugins}
        rehypePlugins={streaming ? baseRehype : fullRehype}
        components={streaming ? streamingComponents : finalComponents}
      >
        {md}
      </ReactMarkdown>
    </div>
  );
});
MarkdownView.displayName = 'MarkdownView';
