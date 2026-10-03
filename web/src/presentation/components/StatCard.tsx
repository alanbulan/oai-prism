import React from 'react';
import { Card, Skeleton, theme } from 'antd';

/** #rrggbb → rgba(r,g,b,a)：图标底色等需要同色系半透明的场景 */
function withAlpha(hex: string, alpha: number): string {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) return hex;
  const n = parseInt(m[1], 16);
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`;
}

interface StatCardProps {
  title: React.ReactNode;
  value: React.ReactNode;
  suffix?: React.ReactNode;
  icon: React.ReactNode;
  /** 强调色（十六进制），用于图标与图标底色 */
  color: string;
  footer?: React.ReactNode;
  loading?: boolean;
}

/** 指标卡：标题 + 图标徽记 + 大号数值 + 附注，统计页与账号页共用 */
export const StatCard: React.FC<StatCardProps> = ({ title, value, suffix, icon, color, footer, loading }) => {
  const { token } = theme.useToken();

  return (
    <Card
      variant="outlined"
      style={{ height: '100%', boxShadow: token.boxShadowTertiary }}
      styles={{ body: { padding: '18px 20px' } }}
    >
      <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 12 }}>
        <div style={{ minWidth: 0 }}>
          <div style={{ color: token.colorTextSecondary, fontSize: 13, marginBottom: 8 }}>{title}</div>
          {loading ? (
            <Skeleton.Input active size="small" style={{ width: 96 }} />
          ) : (
            <div
              style={{
                fontSize: 26,
                fontWeight: 600,
                lineHeight: 1.15,
                color: token.colorTextHeading,
                fontVariantNumeric: 'tabular-nums',
                whiteSpace: 'nowrap',
              }}
            >
              {value}
              {suffix != null && (
                <span style={{ fontSize: 13, fontWeight: 500, color: token.colorTextTertiary, marginLeft: 4 }}>
                  {suffix}
                </span>
              )}
            </div>
          )}
        </div>
        <div
          style={{
            width: 40,
            height: 40,
            borderRadius: 10,
            display: 'grid',
            placeItems: 'center',
            fontSize: 18,
            color,
            background: withAlpha(color, 0.12),
            flexShrink: 0,
          }}
        >
          {icon}
        </div>
      </div>
      {footer && (
        <div
          style={{
            marginTop: 12,
            paddingTop: 10,
            borderTop: `1px dashed ${token.colorBorderSecondary}`,
            fontSize: 12,
            color: token.colorTextTertiary,
            whiteSpace: 'nowrap',
            overflow: 'hidden',
            textOverflow: 'ellipsis',
          }}
        >
          {footer}
        </div>
      )}
    </Card>
  );
};
