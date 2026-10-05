import React, { useEffect, useState } from 'react';
import { Tooltip, Typography, theme } from 'antd';
import { ClockCircleOutlined, LoadingOutlined } from '@ant-design/icons';
import type { ChatMessage } from '../../../domain/chat/entity';
import { formatCompactTime, formatDateTime, formatDuration, formatTokens } from '../../utils/format';

const { Text } = Typography;

/** 今天的消息只显示时分秒，更早的带日期 */
function clock(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return d.toDateString() === new Date().toDateString()
    ? d.toTimeString().slice(0, 8)
    : formatCompactTime(d);
}

/** 生成中的计时：12 秒 / 3 分 05 秒 */
function elapsed(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  return s < 60 ? `${s} 秒` : `${Math.floor(s / 60)} 分 ${String(s % 60).padStart(2, '0')} 秒`;
}

const Dot: React.FC = () => <span style={{ opacity: 0.5 }}>·</span>;

/** 生成中：每秒刷新的已用时 */
const Ticker: React.FC<{ since: string }> = ({ since }) => {
  const start = Date.parse(since);
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);
  return <span>已用时 {elapsed(now - start)}</span>;
};

/**
 * 消息下方的一行附加信息。
 *   user：发送时间；
 *   assistant：完成时间 · 耗时 · 本轮输入 / 输出 tokens；生成中显示实时计时。
 * 早于这个功能的回复没有耗时与用量，只显示时间。
 */
export const MessageMeta: React.FC<{ m: ChatMessage; children?: React.ReactNode }> = ({ m, children }) => {
  const { token } = theme.useToken();
  const style: React.CSSProperties = {
    display: 'inline-flex',
    alignItems: 'center',
    flexWrap: 'wrap',
    gap: 6,
    fontSize: 12,
    color: token.colorTextTertiary,
    fontVariantNumeric: 'tabular-nums',
  };

  if (m.role === 'assistant' && m.status === 'loading') {
    return (
      <span style={style}>
        <LoadingOutlined style={{ color: token.colorPrimary }} />
        <Text type="secondary" style={{ fontSize: 12 }}>
          生成中
        </Text>
        <Dot />
        <Ticker since={m.createdAt} />
      </span>
    );
  }

  const time = (
    <Tooltip title={m.role === 'user' ? `发送于 ${formatDateTime(m.createdAt)}` : `回复于 ${formatDateTime(m.createdAt)}`}>
      <span>{clock(m.createdAt)}</span>
    </Tooltip>
  );
  if (m.role !== 'assistant') return <span style={style}>{time}</span>;

  const u = m.usage;
  return (
    <span style={style}>
      {children}
      {time}
      {m.durationMs != null && (
        <>
          <Dot />
          <Tooltip title={`${m.status === 'error' ? '失败前等待' : '从发送到回复完成'} ${m.durationMs.toLocaleString()} ms`}>
            <span style={{ color: m.status === 'error' ? token.colorError : undefined }}>
              <ClockCircleOutlined style={{ marginRight: 4 }} />
              {m.status === 'error' ? '失败 · ' : ''}
              {formatDuration(m.durationMs)}
            </span>
          </Tooltip>
        </>
      )}
      {u && (
        <>
          <Dot />
          <Tooltip
            title={
              <div style={{ fontVariantNumeric: 'tabular-nums' }}>
                输入 {u.promptTokens.toLocaleString()} tokens（本轮发给模型的完整上下文）
                <br />
                输出 {u.completionTokens.toLocaleString()} tokens（回复 + 思考）
              </div>
            }
          >
            <span>
              输入 {formatTokens(u.promptTokens)} · 输出 {formatTokens(u.completionTokens)}
            </span>
          </Tooltip>
        </>
      )}
    </span>
  );
};
