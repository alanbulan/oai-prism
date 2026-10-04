import React from 'react';
import { Drawer, Descriptions, Tag, Badge, Space, Typography, Divider } from 'antd';
import {
  CrownOutlined,
  ThunderboltOutlined,
  ClockCircleOutlined,
  CheckCircleOutlined,
  CloseCircleOutlined,
} from '@ant-design/icons';
import { useAccountStore } from '../../../application/account/store';
import { formatDateTime } from '../../utils/format';

const { Text } = Typography;

export const PlanDetailDrawer: React.FC = () => {
  const { selectedAccount, detailDrawerOpen, closeDetailDrawer } = useAccountStore();

  if (!selectedAccount) return null;

  const daysRemaining = selectedAccount.expires_in_sec
    ? Math.round(selectedAccount.expires_in_sec / 86400)
    : null;

  return (
    <Drawer
      title={
        <Space>
          <CrownOutlined style={{ color: '#faad14' }} />
          <span>账号详情 - {selectedAccount.name}</span>
        </Space>
      }
      placement="right"
      width={600}
      open={detailDrawerOpen}
      onClose={closeDetailDrawer}
    >
      <Space orientation="vertical" size="middle" style={{ width: '100%' }}>
        <Descriptions title="账号信息" bordered column={1} size="small">
          <Descriptions.Item label="账号 ID">{selectedAccount.id}</Descriptions.Item>
          <Descriptions.Item label="绑定邮箱">
            {selectedAccount.email || <Text type="secondary">未绑定</Text>}
          </Descriptions.Item>
          <Descriptions.Item label="凭据来源">
            <Tag color="purple">{selectedAccount.source}</Tag>
          </Descriptions.Item>
          <Descriptions.Item label="调度状态">
            <Badge
              status={selectedAccount.enabled ? 'success' : 'error'}
              text={selectedAccount.enabled ? '正常调度' : '已暂停'}
            />
          </Descriptions.Item>
        </Descriptions>

        <Divider style={{ margin: '8px 0' }} />

        <Descriptions title="计划与凭据" bordered column={1} size="small">
          <Descriptions.Item label="计划等级">
            <Tag color="gold" icon={<CrownOutlined />}>
              {(selectedAccount.plan || 'pro').toUpperCase()}
            </Tag>
          </Descriptions.Item>
          <Descriptions.Item label="Token 到期">
            {selectedAccount.token_expires ? (
              <Space>
                <ClockCircleOutlined />
                <span>{formatDateTime(selectedAccount.token_expires)}</span>
                {daysRemaining !== null && (
                  <Tag color={daysRemaining > 3 ? 'green' : 'red'}>
                    剩余 {daysRemaining} 天
                  </Tag>
                )}
              </Space>
            ) : (
              <Text type="secondary">无过期时间（静态凭据）</Text>
            )}
          </Descriptions.Item>
          <Descriptions.Item label="凭据构成">
            <Space>
              <Tag icon={selectedAccount.has_access_token ? <CheckCircleOutlined /> : <CloseCircleOutlined />} color={selectedAccount.has_access_token ? 'success' : 'default'}>
                Access Token
              </Tag>
              <Tag icon={selectedAccount.has_session ? <CheckCircleOutlined /> : <CloseCircleOutlined />} color={selectedAccount.has_session ? 'processing' : 'default'}>
                Session Cookie
              </Tag>
              <Tag icon={selectedAccount.has_refresh_token ? <CheckCircleOutlined /> : <CloseCircleOutlined />} color={selectedAccount.has_refresh_token ? 'purple' : 'default'}>
                Refresh Token
              </Tag>
            </Space>
          </Descriptions.Item>
        </Descriptions>

        <Divider style={{ margin: '8px 0' }} />

        <Descriptions title="运行统计" bordered column={2} size="small">
          <Descriptions.Item label="在途请求">
            <Text strong type={selectedAccount.inflight > 0 ? 'success' : 'secondary'}>
              {selectedAccount.inflight}
            </Text>
          </Descriptions.Item>
          <Descriptions.Item label="并发上限">
            <Tag color="cyan" icon={<ThunderboltOutlined />}>
              {selectedAccount.max_concurrency > 0 ? `${selectedAccount.max_concurrency} 槽位` : '不限'}
            </Tag>
          </Descriptions.Item>
          <Descriptions.Item label="累计请求">
            {selectedAccount.total_requests} 次
          </Descriptions.Item>
          <Descriptions.Item label="失败请求">
            <Text type={selectedAccount.failures > 0 ? 'danger' : 'secondary'}>
              {selectedAccount.failures} 次
            </Text>
          </Descriptions.Item>
          <Descriptions.Item label="连续失败">
            {selectedAccount.fail_streak} 次
          </Descriptions.Item>
          <Descriptions.Item label="冷却剩余">
            {selectedAccount.cooldown_sec > 0 ? `${Math.round(selectedAccount.cooldown_sec)} 秒` : '无'}
          </Descriptions.Item>
        </Descriptions>

        {selectedAccount.tags && selectedAccount.tags.length > 0 && (
          <div>
            <Text type="secondary" style={{ fontSize: 12 }}>标签</Text>
            <div style={{ marginTop: 4 }}>
              {selectedAccount.tags.map((t, idx) => (
                <Tag key={idx}>{t}</Tag>
              ))}
            </div>
          </div>
        )}
      </Space>
    </Drawer>
  );
};
