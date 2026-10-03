import React, { useState } from 'react';
import { Modal, Form, Input, Button, message, theme } from 'antd';
import { LockOutlined, UserOutlined } from '@ant-design/icons';
import { httpClient, setApiKey } from '../../infrastructure/http/client';
import { BrandLogo } from './BrandLogo';

interface AdminLoginModalProps {
  open: boolean;
  onClose: () => void;
  onSuccess?: () => void;
}

export const AdminLoginModal: React.FC<AdminLoginModalProps> = ({ open, onClose, onSuccess }) => {
  const { token } = theme.useToken();
  const [loading, setLoading] = useState(false);
  const [form] = Form.useForm();

  const handleFinish = async (values: any) => {
    setLoading(true);
    try {
      const res = await httpClient.post<any>('/admin/login', {
        username: values.username,
        password: values.password,
      });
      if (res.data?.status === 'ok' && res.data?.token) {
        // 管理会话令牌：后续 /admin 与 /v1 请求都以 Bearer 携带
        setApiKey(res.data.token);
        message.success('管理员认证成功！已载入完整运维权限。');
        onSuccess?.();
        onClose();
      }
    } catch (err: any) {
      message.error(err?.message || '登录校验失败，请检查账号密码');
    } finally {
      setLoading(false);
    }
  };

  return (
    <Modal open={open} onCancel={onClose} footer={null} width={400} destroyOnHidden>
      <div style={{ margin: '12px 0 24px', textAlign: 'center' }}>
        <BrandLogo size={48} style={{ margin: '0 auto 14px' }} />
        <div style={{ fontSize: 18, fontWeight: 600, color: token.colorTextHeading }}>管理员登录</div>
        <div style={{ fontSize: 13, color: token.colorTextSecondary, marginTop: 6, lineHeight: 1.6 }}>
          密码由网关配置 <code>server.admin_password</code> 设定
          <br />
          本机访问使用 API Key 即可管理，无需登录
        </div>
      </div>

      <Form
        form={form}
        layout="vertical"
        onFinish={handleFinish}
        initialValues={{ username: 'admin', password: '' }}
      >
        <Form.Item
          label="管理员账号"
          name="username"
          rules={[{ required: true, message: '请输入管理员账号' }]}
        >
          <Input prefix={<UserOutlined style={{ color: token.colorTextQuaternary }} />} placeholder="admin" />
        </Form.Item>

        <Form.Item
          label="管理密码"
          name="password"
          rules={[{ required: true, message: '请输入管理密码' }]}
        >
          <Input.Password
            prefix={<LockOutlined style={{ color: token.colorTextQuaternary }} />}
            placeholder="server.admin_password"
          />
        </Form.Item>

        <Form.Item style={{ marginTop: 24, marginBottom: 8 }}>
          <Button type="primary" htmlType="submit" loading={loading} block style={{ height: 40 }}>
            登录
          </Button>
        </Form.Item>
      </Form>
    </Modal>
  );
};
