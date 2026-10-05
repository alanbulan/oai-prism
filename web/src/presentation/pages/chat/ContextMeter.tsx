import React from 'react';
import { Popover, Progress, Typography, theme } from 'antd';
import type { ChatMessage } from '../../../domain/chat/entity';
import { formatTokens } from '../../utils/format';

const { Text } = Typography;

/** 官方公布的模型上下文窗口。上游在内部压缩长会话，超出后对话照样能继续（实测 1546 万 tokens 仍逐字记得） */
const NOMINAL_WINDOW = 1_000_000;

function percentText(p: number): string {
  if (p <= 0) return '0%';
  if (p < 0.1) return '<0.1%';
  return `${p < 10 ? p.toFixed(1) : Math.round(p)}%`;
}

const Row: React.FC<{ label: string; children: React.ReactNode }> = ({ label, children }) => (
  <div style={{ display: 'flex', justifyContent: 'space-between', gap: 16, fontSize: 12, lineHeight: '22px' }}>
    <Text type="secondary" style={{ fontSize: 12 }}>
      {label}
    </Text>
    <span style={{ fontVariantNumeric: 'tabular-nums' }}>{children}</span>
  </div>
);

/**
 * 输入框旁的上下文用量环：环 = 窗口占用，数字 = 占用的 token 数；悬停看明细。
 *
 * 占用取最近一轮的输入 + 输出：输入是这一轮发给模型的完整上下文（历史 + 新消息），
 * 输出下一轮会进入历史 —— 两者之和就是此刻会话占着的窗口。
 */
export const ContextMeter: React.FC<{ messages: ChatMessage[] }> = ({ messages }) => {
  const { token } = theme.useToken();
  const replies = messages.filter((m) => m.role === 'assistant' && m.usage);
  const last = replies[replies.length - 1]?.usage;
  const used = last ? last.promptTokens + last.completionTokens : 0;
  const pct = (used / NOMINAL_WINDOW) * 100;
  const color = pct < 50 ? token.colorPrimary : pct < 80 ? token.colorWarning : token.colorError;
  const total = replies.reduce(
    (a, m) => ({ input: a.input + m.usage!.promptTokens, output: a.output + m.usage!.completionTokens }),
    { input: 0, output: 0 },
  );

  const detail = (
    <div style={{ width: 268 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline' }}>
        <Text strong>上下文窗口</Text>
        <Text type="secondary" style={{ fontSize: 12 }}>
          {percentText(pct)}
        </Text>
      </div>
      <Progress percent={Math.min(100, pct)} showInfo={false} strokeColor={color} size="small" style={{ margin: '4px 0 2px' }} />
      <div style={{ fontSize: 12, marginBottom: 8, fontVariantNumeric: 'tabular-nums' }}>
        {used.toLocaleString()} / {NOMINAL_WINDOW.toLocaleString()} tokens
      </div>
      {last ? (
        <>
          <Row label="最近一轮">
            输入 {last.promptTokens.toLocaleString()} · 输出 {last.completionTokens.toLocaleString()}
          </Row>
          <Row label={`本会话累计（${replies.length} 轮）`}>
            {formatTokens(total.input + total.output)}
          </Row>
          <Row label="　其中输入 / 输出">
            {formatTokens(total.input)} / {formatTokens(total.output)}
          </Row>
        </>
      ) : (
        <Text type="secondary" style={{ fontSize: 12 }}>
          这个会话还没有完成的回复。
        </Text>
      )}
      <div style={{ marginTop: 8, fontSize: 12, color: token.colorTextTertiary, lineHeight: 1.6 }}>
        输入是每一轮发给模型的完整上下文（历史 + 新消息），网关按实际内容计数，所以累计会随轮数增长。
        窗口按官方公布的 100 万计；超出后上游在内部压缩，对话照样能继续。
      </div>
    </div>
  );

  return (
    <Popover content={detail} placement="topRight" mouseEnterDelay={0.15}>
      <span
        style={{
          display: 'inline-flex',
          alignItems: 'center',
          gap: 6,
          padding: '2px 8px',
          borderRadius: 12,
          fontSize: 12,
          color: token.colorTextSecondary,
          cursor: 'default',
          fontVariantNumeric: 'tabular-nums',
        }}
        aria-label={`上下文占用 ${used} tokens`}
      >
        {/* 占比很小时也留一点弧，让人看得出这是个进度环 */}
        <Progress
          type="circle"
          percent={used > 0 ? Math.max(3, Math.min(100, pct)) : 0}
          size={16}
          strokeWidth={16}
          showInfo={false}
          strokeColor={color}
        />
        {formatTokens(used)}
      </span>
    </Popover>
  );
};
