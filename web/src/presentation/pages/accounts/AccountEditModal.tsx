import React, { useEffect } from 'react';
import { Modal, Form, Input, Select, InputNumber, Switch, message } from 'antd';
import { useAccountStore } from '../../../application/account/store';
import { planKey, planOptions } from '../../utils/plan';

export const AccountEditModal: React.FC = () => {
  const { editingAccount, editModalOpen, closeEditModal, updateAccount } = useAccountStore();
  const [form] = Form.useForm();

  useEffect(() => {
    if (editingAccount) {
      form.setFieldsValue({
        name: editingAccount.name,
        email: editingAccount.email,
        plan: planKey(editingAccount.plan) || 'pro',
        max_concurrency: editingAccount.max_concurrency || 2,
        enabled: !editingAccount.disabled,
        priority: editingAccount.priority ?? 0,
      });
    }
  }, [editingAccount, form]);

  const handleOk = async () => {
    try {
      const values = await form.validateFields();
      if (!editingAccount) return;
      await updateAccount(editingAccount.id, values);
      message.success(`账号 [${editingAccount.name}] 配置已持久化更新！`);
      closeEditModal();
    } catch (err: any) {
      if (err.errorFields) return;
      message.error(err.message || '更新失败');
    }
  };

  return (
    <Modal
      title={`编辑账号配置 - ${editingAccount?.id}`}
      open={editModalOpen}
      onOk={handleOk}
      onCancel={closeEditModal}
      okText="保存并更新"
      cancelText="取消"
      destroyOnClose
    >
      <Form form={form} layout="vertical">
        <Form.Item
          name="name"
          label="账号展示名称"
          rules={[{ required: true, message: '请输入账号名称' }]}
        >
          <Input placeholder="例如：团队主力账号" />
        </Form.Item>

        <Form.Item name="email" label="绑定邮箱">
          <Input placeholder="user@example.com" />
        </Form.Item>

        <Form.Item name="plan" label="上游计划等级">
          <Select options={planOptions(editingAccount?.plan)} />
        </Form.Item>

        <Form.Item
          name="max_concurrency"
          label="最大并发槽位"
          extra="设置允许同时在上游执行推理的请求数，超出将在网关排队调度"
        >
          <InputNumber min={1} max={64} style={{ width: '100%' }} />
        </Form.Item>

        <Form.Item
          name="priority"
          label="调度优先级"
          extra="数值越大越先用（默认 0）。同一优先级内按调度策略分摊；高优先级的账号都不可用（冷却、满载、凭据失效）时才用低优先级的。已在进行中的会话继续用原来的账号"
        >
          <InputNumber min={-100} max={100} precision={0} style={{ width: '100%' }} />
        </Form.Item>

        <Form.Item
          name="enabled"
          label="参与调度"
          valuePropName="checked"
          extra="关闭后不再接新请求（进行中的请求照常完成），账号保留在列表里，随时可以再打开"
        >
          <Switch checkedChildren="启用" unCheckedChildren="停用" />
        </Form.Item>
      </Form>
    </Modal>
  );
};
