import React, { useState } from 'react';
import { Button, Tooltip, theme } from 'antd';
import { CheckOutlined, CopyOutlined, DownOutlined, UpOutlined, LoadingOutlined } from '@ant-design/icons';

/** 超过这个行数的代码默认折叠，避免一段长代码把对话撑得看不到头尾 */
const COLLAPSE_LINES = 28;

interface CodeBlockProps {
  lang: string;
  code: string;
  /** 已高亮的 <code> 元素（react-markdown 渲染结果） */
  children: React.ReactNode;
  /** 流式生成中：显示进度标记，不折叠 */
  streaming?: boolean;
  /** 嵌在预览卡片里时不再画外框与标题栏 */
  bare?: boolean;
  /** 标题栏右侧的附加操作（如"渲染结果"切回预览） */
  extra?: React.ReactNode;
}

/** 代码块：语言标签 + 行数 + 一键复制 + 长代码折叠 */
export const CodeBlock: React.FC<CodeBlockProps> = ({ lang, code, children, streaming, bare, extra }) => {
  const { token } = theme.useToken();
  const [copied, setCopied] = useState(false);
  const lines = code ? code.split('\n').length : 0;
  const collapsible = !streaming && lines > COLLAPSE_LINES;
  const [expanded, setExpanded] = useState(false);
  const collapsed = collapsible && !expanded;

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // 剪贴板权限被拒（非安全上下文）：静默忽略，用户仍可手动选择复制
    }
  };

  const body = (
    <div style={{ position: 'relative' }}>
      <pre className="op-code" style={collapsed ? { maxHeight: COLLAPSE_LINES * 20 + 24, overflow: 'hidden' } : undefined}>
        {children}
      </pre>
      {collapsible && (
        <div className={collapsed ? 'op-code-fold op-code-fold--collapsed' : 'op-code-fold'}>
          <Button
            size="small"
            type="text"
            icon={collapsed ? <DownOutlined /> : <UpOutlined />}
            onClick={() => setExpanded(!expanded)}
          >
            {collapsed ? `展开全部 ${lines} 行` : '收起'}
          </Button>
        </div>
      )}
    </div>
  );

  if (bare) return body;

  return (
    <div className="op-code-card">
      <div className="op-code-head">
        <span className="op-code-lang">{lang || 'text'}</span>
        <span style={{ color: token.colorTextQuaternary }}>{lines} 行</span>
        {streaming && (
          <span style={{ color: token.colorPrimary }}>
            <LoadingOutlined /> 生成中
          </span>
        )}
        <span style={{ marginInlineStart: 'auto' }}>{extra}</span>
        <Tooltip title={copied ? '已复制' : '复制代码'}>
          <Button
            type="text"
            size="small"
            icon={copied ? <CheckOutlined style={{ color: token.colorSuccess }} /> : <CopyOutlined />}
            onClick={copy}
          />
        </Tooltip>
      </div>
      {body}
    </div>
  );
};
