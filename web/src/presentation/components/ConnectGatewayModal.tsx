import React, { useState } from 'react';
import { Modal, Input, Button, Alert, Typography, message, theme } from 'antd';
import { KeyOutlined, SafetyCertificateOutlined } from '@ant-design/icons';
import { httpClient, setApiKey } from '../../infrastructure/http/client';
import { useAuthStore } from '../../application/auth/store';
import { BrandLogo } from './BrandLogo';

const { Text } = Typography;

interface ConnectGatewayModalProps {
  /** 切换到管理员密码登录 */
  onAdminLogin: () => void;
}

/**
 * 连接网关：浏览器里没有可用凭据时的引导。
 * 候选 Key 先对 /admin/stats 校验，通过后才写入本地 —— 避免存进一个无效 Key。
 */
export const ConnectGatewayModal: React.FC<ConnectGatewayModalProps> = ({ onAdminLogin }) => {
  const { token } = theme.useToken();
  const { promptOpen, reason, closePrompt } = useAuthStore();
  const [key, setKey] = useState('');
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState('');

  const handleConnect = async () => {
    const k = key.trim();
    if (!k) {
      setError('请填入 API Key');
      return;
    }
    setChecking(true);
    setError('');
    try {
      await httpClient.get('/admin/stats', { headers: { Authorization: `Bearer ${k}` } });
      setApiKey(k);
      setKey('');
      message.success('已连接网关，数据已刷新');
    } catch (err: any) {
      setError(err?.message || '校验失败');
    } finally {
      setChecking(false);
    }
  };

  return (
    <Modal open={promptOpen} onCancel={closePrompt} footer={null} width={440} destroyOnHidden>
      <div style={{ margin: '12px 0 20px', textAlign: 'center' }}>
        <BrandLogo size={48} style={{ margin: '0 auto 14px' }} />
        <div style={{ fontSize: 18, fontWeight: 600, color: token.colorTextHeading }}>连接网关</div>
        <div style={{ fontSize: 13, color: token.colorTextSecondary, marginTop: 6, lineHeight: 1.6 }}>
          账号、统计与调试台需要有效的 API Key 才能读取
          {reason && (
            <>
              <br />
              <Text type="warning" style={{ fontSize: 12 }}>网关返回：{reason}</Text>
            </>
          )}
        </div>
      </div>

      <Input.Password
        autoFocus
        size="large"
        prefix={<KeyOutlined style={{ color: token.colorTextQuaternary }} />}
        placeholder="粘贴 API Key，例如 sk-prism-…"
        value={key}
        onChange={(e) => {
          setKey(e.target.value);
          if (error) setError('');
        }}
        onPressEnter={handleConnect}
        status={error ? 'error' : undefined}
      />
      {error && <Alert type="error" showIcon title={error} style={{ marginTop: 10 }} />}

      <Button type="primary" block loading={checking} onClick={handleConnect} style={{ height: 40, marginTop: 16 }}>
        连接
      </Button>

      <div
        style={{
          marginTop: 20,
          padding: '12px 14px',
          borderRadius: 10,
          background: token.colorFillQuaternary,
          fontSize: 12,
          lineHeight: 1.8,
          color: token.colorTextSecondary,
        }}
      >
        <div>
          <Text strong style={{ fontSize: 12 }}>本机访问</Text>：任一有效 Key 均可，Codex 正在使用的那个也行
        </div>
        <div>
          <Text strong style={{ fontSize: 12 }}>远程访问</Text>：配置文件 <Text code style={{ fontSize: 11 }}>facade.api_keys</Text>{' '}
          中的 Key，或管理员密码登录
        </div>
        <div style={{ color: token.colorTextTertiary }}>Key 只保存在当前浏览器（按访问地址区分）</div>
      </div>

      <div style={{ textAlign: 'center', marginTop: 12 }}>
        <Button
          type="link"
          size="small"
          icon={<SafetyCertificateOutlined />}
          onClick={() => {
            closePrompt();
            onAdminLogin();
          }}
        >
          使用管理员密码登录
        </Button>
      </div>
    </Modal>
  );
};
