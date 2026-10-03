import React, { useState } from 'react';
import { Modal, Form, Input, Button, message, Space, Tag } from 'antd';
import { LockOutlined, UserOutlined, SafetyCertificateOutlined } from '@ant-design/icons';
import { httpClient, setApiKey } from '../../infrastructure/http/client';

interface AdminLoginModalProps {
  open: boolean;
  onClose: () => void;
  onSuccess?: () => void;
}

export const AdminLoginModal: React.FC<AdminLoginModalProps> = ({ open, onClose, onSuccess }) => {
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
    <Modal
      title={
        <Space>
          <SafetyCertificateOutlined style={{ color: '#52c41a' }} />
          <span>管理人登录认证</span>
        </Space>
      }
      open={open}
      onCancel={onClose}
      footer={null}
      width={420}
      destroyOnHidden
    >
      <div style={{ margin: '16px 0 20px', textAlign: 'center' }}>
        <div style={{ fontSize: 13, color: '#666' }}>
          请输入超级管理员凭据以进行配置与安全操作
        </div>
        <Tag color="blue" style={{ marginTop: 6 }}>
          密码由网关配置 server.admin_password 设定；本机用 API Key 即可管理
        </Tag>
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
          <Input prefix={<UserOutlined style={{ color: '#aaa' }} />} placeholder="admin" />
        </Form.Item>

        <Form.Item
          label="管理密码"
          name="password"
          rules={[{ required: true, message: '请输入管理密码' }]}
        >
          <Input.Password
            prefix={<LockOutlined style={{ color: '#aaa' }} />}
            placeholder="server.admin_password"
          />
        </Form.Item>

        <Form.Item style={{ marginTop: 24, marginBottom: 8 }}>
          <Button type="primary" htmlType="submit" loading={loading} block style={{ height: 40 }}>
            立即登录认证
          </Button>
        </Form.Item>
      </Form>
    </Modal>
  );
};
