import React from 'react';
import { Drawer, Descriptions, Tag, Typography, Alert, Tooltip, theme } from 'antd';
import { ClockCircleOutlined, FieldTimeOutlined, RobotOutlined, UserOutlined } from '@ant-design/icons';
import type { RequestLog } from '../../../domain/statistics/entity';
import { useAccountStore } from '../../../application/account/store';
import { MONO_FAMILY } from '../../theme/tokens';
import { formatDateTime, formatDuration, formatRelative, normalizeIp, parseClient } from '../../utils/format';
import { latencyTone, statusTagColor, statusText } from './logMeta';

const { Text } = Typography;

interface RequestLogDrawerProps {
  /** 关闭时保留上一条记录，让收起动画期间内容不闪空 */
  log: RequestLog | null;
  open: boolean;
  onClose: () => void;
}

/** 请求详情：顶部摘要 + 三项关键指标 + 分组明细，原始值（毫秒、完整 UA）一并保留 */
export const RequestLogDrawer: React.FC<RequestLogDrawerProps> = ({ log, open, onClose }) => (
  <Drawer
    title="请求详情"
    open={open && Boolean(log)}
    onClose={onClose}
    size={520}
    styles={{ body: { paddingTop: 16 } }}
  >
    {log && <LogDetail log={log} />}
  </Drawer>
);

const LogDetail: React.FC<{ log: RequestLog }> = ({ log }) => {
  const { token } = theme.useToken();
  const accountNameMap = useAccountStore((s) => s.accountNameMap);

  const toneColor = {
    fast: token.colorSuccess,
    normal: token.colorText,
    slow: token.colorWarning,
    'very-slow': token.colorError,
  } as const;

  const sectionTitle = (text: string) => (
    <div
      style={{
        fontSize: 12,
        fontWeight: 600,
        letterSpacing: 0.4,
        color: token.colorTextTertiary,
        margin: '22px 0 8px',
      }}
    >
      {text}
    </div>
  );

  const descStyles = {
    label: { width: 96, color: token.colorTextSecondary, whiteSpace: 'nowrap' as const, paddingBottom: 6 },
    content: { color: token.colorText, minWidth: 0, paddingBottom: 6 },
  };

  const ok = log.statusCode >= 200 && log.statusCode < 300;
  const client = parseClient(log.userAgent);
  const { ip, local } = normalizeIp(log.clientIp);
  const accountName = log.accountId ? accountNameMap.get(log.accountId) : undefined;
  const tokens = log.promptTokens + log.completionTokens;
  // 0 用量只有两种来源：失败请求不计，或用量统计上线前的历史记录
  const noUsageHint = ok ? '未记录（统计上线前的请求）' : '失败请求不计用量';
  const accountSub = !log.accountId
    ? '未进入调度'
    : accountName && accountName !== log.accountId
      ? accountName
      : '上游账号';

  return (
    <>
      {/* 摘要：状态 + 接口 + 时间 */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
        <Tag color={statusTagColor(log.statusCode)} style={{ marginInlineEnd: 0, fontWeight: 600 }}>
          {log.statusCode} {statusText(log.statusCode)}
        </Tag>
        <Tag bordered={false} style={{ marginInlineEnd: 0, fontFamily: MONO_FAMILY, fontSize: 11 }}>
          {log.method}
        </Tag>
        <span
          style={{
            fontFamily: MONO_FAMILY,
            fontSize: 15,
            fontWeight: 600,
            color: token.colorTextHeading,
            wordBreak: 'break-all',
          }}
        >
          {log.path}
        </span>
      </div>
      <div style={{ marginTop: 8, fontSize: 12, color: token.colorTextTertiary }}>
        <ClockCircleOutlined /> {formatDateTime(log.timestamp)} · {formatRelative(log.timestamp)}
      </div>

      {/* 关键指标 */}
      <div
        style={{
          marginTop: 18,
          display: 'grid',
          gridTemplateColumns: 'repeat(3, minmax(0, 1fr))',
          border: `1px solid ${token.colorBorderSecondary}`,
          borderRadius: 12,
          overflow: 'hidden',
        }}
      >
        {[
          {
            icon: <FieldTimeOutlined />,
            label: '耗时',
            value: (
              <span style={{ color: toneColor[latencyTone(log.durationMs)] }}>{formatDuration(log.durationMs)}</span>
            ),
            sub: `${log.durationMs.toLocaleString()} ms`,
          },
          {
            icon: <RobotOutlined />,
            label: '模型',
            value: log.model || '—',
            sub: tokens > 0 ? `${tokens.toLocaleString()} tokens` : ok ? '用量未记录' : '失败不计用量',
          },
          {
            icon: <UserOutlined />,
            label: '账号',
            value: log.accountId || '未分配',
            sub: accountSub,
          },
        ].map((m, i) => (
          <div
            key={m.label}
            style={{
              padding: '12px 14px',
              minWidth: 0,
              borderLeft: i ? `1px solid ${token.colorBorderSecondary}` : 'none',
              background: token.colorFillQuaternary,
            }}
          >
            <div style={{ fontSize: 12, color: token.colorTextTertiary }}>
              {m.icon} {m.label}
            </div>
            <div
              style={{
                marginTop: 4,
                fontSize: 15,
                fontWeight: 600,
                color: token.colorTextHeading,
                whiteSpace: 'nowrap',
                overflow: 'hidden',
                textOverflow: 'ellipsis',
                fontVariantNumeric: 'tabular-nums',
              }}
              title={typeof m.value === 'string' ? m.value : undefined}
            >
              {m.value}
            </div>
            <div
              style={{
                marginTop: 2,
                fontSize: 12,
                color: token.colorTextTertiary,
                whiteSpace: 'nowrap',
                overflow: 'hidden',
                textOverflow: 'ellipsis',
              }}
            >
              {m.sub}
            </div>
          </div>
        ))}
      </div>

      {!ok && log.errorMessage && (
        <Alert
          type={log.statusCode >= 500 ? 'error' : 'warning'}
          showIcon
          title="错误信息"
          description={<div style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{log.errorMessage}</div>}
          style={{ marginTop: 16 }}
        />
      )}

      {sectionTitle('请求')}
      <Descriptions column={1} size="small" colon={false} styles={descStyles}>
        <Descriptions.Item label="请求 ID">
          <Text copyable style={{ fontFamily: MONO_FAMILY, fontSize: 12 }}>
            {log.id}
          </Text>
        </Descriptions.Item>
        <Descriptions.Item label="请求时间">
          <span style={{ fontVariantNumeric: 'tabular-nums' }}>{formatDateTime(log.timestamp)}</span>
        </Descriptions.Item>
        <Descriptions.Item label="状态码">
          {log.statusCode} {statusText(log.statusCode)}
        </Descriptions.Item>
        <Descriptions.Item label="Token 用量">
          {tokens > 0 ? (
            <span style={{ fontVariantNumeric: 'tabular-nums' }}>
              输入 {log.promptTokens.toLocaleString()} · 输出 {log.completionTokens.toLocaleString()}
            </span>
          ) : (
            <Text type="secondary">{noUsageHint}</Text>
          )}
        </Descriptions.Item>
        {ok && log.errorMessage && <Descriptions.Item label="备注">{log.errorMessage}</Descriptions.Item>}
      </Descriptions>

      {sectionTitle('客户端')}
      <Descriptions column={1} size="small" colon={false} styles={descStyles}>
        <Descriptions.Item label="调用方">
          {client.name}
          {client.version && (
                  <Text type="secondary" style={{ marginInlineStart: 6 }}>
                    {client.version}
                  </Text>
                )}
        </Descriptions.Item>
        <Descriptions.Item label="来源地址">
          <span style={{ fontFamily: MONO_FAMILY, fontSize: 12 }}>{ip}</span>
          {local && (
            <Tooltip title="回环地址：请求来自网关所在机器">
              <Tag bordered={false} style={{ marginInlineStart: 8 }}>
                本机
              </Tag>
            </Tooltip>
          )}
        </Descriptions.Item>
        <Descriptions.Item label="User-Agent">
          {log.userAgent ? (
            <pre className="op-mono-block" style={{ width: '100%' }}>
              {log.userAgent}
            </pre>
          ) : (
            <Text type="secondary">未携带</Text>
          )}
        </Descriptions.Item>
      </Descriptions>
    </>
  );
};
