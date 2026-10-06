import React, { useEffect, useState } from 'react';
import { Modal, Table, Button, Space, Typography, message, Input, Tabs, Popconfirm, theme } from 'antd';
import { KeyOutlined, CopyOutlined, PlusOutlined, DeleteOutlined, CodeOutlined } from '@ant-design/icons';
import { getApiKey, setApiKey, httpClient } from '../../infrastructure/http/client';
import { formatDateTime } from '../utils/format';
import { defaultMainModel } from '../../domain/modelFilter';

const { Text, Paragraph } = Typography;

interface ApiKeyModalProps {
  open: boolean;
  onClose: () => void;
}

interface ApiKeyItem {
  key: string;
  name: string;
  created_at: string;
}

export const ApiKeyModal: React.FC<ApiKeyModalProps> = ({ open, onClose }) => {
  const { token } = theme.useToken();
  const [keys, setKeys] = useState<ApiKeyItem[]>([]);
  const [loading, setLoading] = useState(false);
  const [newKeyName, setNewKeyName] = useState('');
  const [manualKey, setManualKey] = useState('');
  const [loadError, setLoadError] = useState('');
  // 示例里的模型名：网关当前的默认模型（上游在售清单第一个），不写死
  const [exampleModel, setExampleModel] = useState('');

  const fetchKeys = async (): Promise<ApiKeyItem[]> => {
    setLoading(true);
    try {
      const res = await httpClient.get<ApiKeyItem[]>('/admin/apikeys');
      const list = res.data || [];
      setKeys(list);
      setLoadError('');
      return list;
    } catch (err: any) {
      // 已启用鉴权且浏览器里还没有可用 Key：提示手动填入
      setKeys([]);
      setLoadError(err?.message || '加载失败');
      return [];
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (open) {
      fetchKeys();
      httpClient
        .get<any>('/v1/models')
        .then((res) => setExampleModel(defaultMainModel(Array.isArray(res.data?.data) ? res.data.data : []) ?? ''))
        .catch(() => setExampleModel(''));
    }
  }, [open]);

  const handleCreate = async () => {
    try {
      await httpClient.post('/admin/apikeys', { name: newKeyName || '新访问密钥' });
      message.success('已生成新的对外 API 密钥！');
      setNewKeyName('');
      // 生成即设为默认：页面内所有 /v1 与 /admin 请求自动携带
      // （fetchKeys 返回最新列表，避免 stale closure 拿到旧 keys）
      const list = await fetchKeys();
      if (list[0]?.key) setApiKey(list[0].key);
    } catch (err: any) {
      message.error(`生成失败: ${err.message}`);
    }
  };

  const handleDelete = async (k: string) => {
    try {
      await httpClient.delete(`/admin/apikeys/${k}`);
      message.success('已注销该密钥');
      fetchKeys();
    } catch (err: any) {
      message.error(`删除失败: ${err.message}`);
    }
  };

  const handleUseManualKey = async () => {
    const k = manualKey.trim();
    if (!k) return;
    setApiKey(k);
    setManualKey('');
    message.success('已保存到浏览器，之后的请求将携带此 Key');
    await fetchKeys();
  };

  const handleCopy = (text: string) => {
    navigator.clipboard.writeText(text);
    message.success('已复制到剪贴板！');
  };

  const columns = [
    {
      title: '密钥名称',
      dataIndex: 'name',
      key: 'name',
      render: (name: string) => <Text strong>{name}</Text>,
    },
    {
      title: 'API Key (Bearer Token)',
      dataIndex: 'key',
      key: 'key',
      render: (k: string) => (
        <Space>
          <Text code style={{ fontSize: 13 }}>{k}</Text>
          <Button
            type="text"
            size="small"
            icon={<CopyOutlined />}
            onClick={() => handleCopy(k)}
          />
        </Space>
      ),
    },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      key: 'created_at',
      render: (t: string) => (
        <Text type="secondary" style={{ fontSize: 12 }}>
          {formatDateTime(t)}
        </Text>
      ),
    },
    {
      title: '操作',
      key: 'action',
      render: (_: any, r: ApiKeyItem) => (
        <Space>
          <Popconfirm
            title="设为当前使用的密钥？"
            description="页面内所有请求将改用此 Key（保存在浏览器本地）。"
            onConfirm={() => {
              setApiKey(r.key);
              message.success('已设为当前使用的密钥');
            }}
            okText="确定"
            cancelText="取消"
          >
            <Button type="link" size="small" icon={<KeyOutlined />}>
              {getApiKey() === r.key ? '使用中' : '设为默认'}
            </Button>
          </Popconfirm>
          <Popconfirm
            title="确定注销此密钥？"
            description="注销后使用此 Key 的外部客户端将无法调用！"
            onConfirm={() => handleDelete(r.key)}
            okText="确定"
            cancelText="取消"
            okButtonProps={{ danger: true }}
          >
            <Button type="link" danger size="small" icon={<DeleteOutlined />}>
              注销
            </Button>
          </Popconfirm>
        </Space>
      ),
    },
  ];

  const firstKey = getApiKey() || keys[0]?.key || '<你的 API Key>';
  const model = exampleModel || '<模型名，见 GET /v1/models>';

  const curlExample = `curl http://localhost:8787/v1/chat/completions \\
  -H "Content-Type: application/json" \\
  -H "Authorization: Bearer ${firstKey}" \\
  -d '{
    "model": "${model}",
    "messages": [{"role": "user", "content": "你好！"}]
  }'`;

  const pythonExample = `from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8787/v1",
    api_key="${firstKey}"
)

resp = client.chat.completions.create(
    model="${model}",
    messages=[{"role": "user", "content": "你好！"}]
)
print(resp.choices[0].message.content)`;

  const codexExample = `# ~/.codex/config.toml
model_provider = "oaiprism"
model = "${model}"
model_context_window = 16384

[model_providers.oaiprism]
name = "oaiprism"
wire_api = "responses"
requires_openai_auth = false
base_url = "http://localhost:8787/v1"
experimental_bearer_token = "${firstKey}"`;

  const preStyle: React.CSSProperties = {
    background: 'var(--op-code-bg)',
    border: `1px solid ${token.colorBorderSecondary}`,
    padding: 12,
    borderRadius: 8,
    fontSize: 12,
    marginTop: 6,
    overflowX: 'auto',
  };

  return (
    <Modal
      title={
        <Space>
          <KeyOutlined style={{ color: token.colorPrimary }} />
          <span>对外 API Key 管理与接入指引</span>
        </Space>
      }
      open={open}
      onCancel={onClose}
      footer={[
        <Button key="close" type="primary" onClick={onClose}>
          完成
        </Button>,
      ]}
      width={780}
      destroyOnHidden
    >
      <Tabs
        defaultActiveKey="keys"
        items={[
          {
            key: 'keys',
            label: '密钥列表',
            children: (
              <div>
                {loadError && (
                  <Paragraph type="danger" style={{ marginBottom: 12 }}>
                    无法读取密钥列表：{loadError}。网关已启用鉴权时，请在下方填入一个有效的 API Key
                    （远程访问管理功能需配置文件中的 Key 或管理员登录）。
                  </Paragraph>
                )}
                <div style={{ display: 'flex', gap: 12, marginBottom: 12 }}>
                  <Input.Password
                    placeholder="已有 API Key：粘贴后保存到本浏览器"
                    value={manualKey}
                    onChange={(e) => setManualKey(e.target.value)}
                    onPressEnter={handleUseManualKey}
                  />
                  <Button icon={<KeyOutlined />} onClick={handleUseManualKey}>
                    使用此 Key
                  </Button>
                </div>
                <div style={{ display: 'flex', gap: 12, marginBottom: 16 }}>
                  <Input
                    placeholder="输入新 Key 描述名称 (例如: 生产环境客户端 / 本地Codex)"
                    value={newKeyName}
                    onChange={(e) => setNewKeyName(e.target.value)}
                    onPressEnter={handleCreate}
                  />
                  <Button type="primary" icon={<PlusOutlined />} onClick={handleCreate}>
                    生成新 Key
                  </Button>
                </div>
                <Table
                  rowKey="key"
                  columns={columns}
                  dataSource={keys}
                  loading={loading}
                  pagination={false}
                  size="small"
                />
              </div>
            ),
          },
          {
            key: 'quickstart',
            label: '客户端接入代码示例',
            children: (
              <div>
                <Paragraph type="secondary">
                  OAIprism 100% 兼容 OpenAI 格式。使用任意 OpenAI SDK 或客户端，将 Base URL 设定为{' '}
                  <Text code>http://localhost:8787/v1</Text> 并在 Header 传入上述生成的 API Key 即可：
                </Paragraph>

                <div style={{ marginTop: 12 }}>
                  <Text strong><CodeOutlined /> 1. cURL 命令行调用示例：</Text>
                  <pre style={preStyle}>
                    {curlExample}
                  </pre>
                  <Button size="small" icon={<CopyOutlined />} onClick={() => handleCopy(curlExample)}>
                    复制 cURL 示例
                  </Button>
                </div>

                <div style={{ marginTop: 16 }}>
                  <Text strong><CodeOutlined /> 2. Python (openai-python) 接入：</Text>
                  <pre style={preStyle}>
                    {pythonExample}
                  </pre>
                </div>

                <div style={{ marginTop: 16 }}>
                  <Text strong><CodeOutlined /> 3. Codex CLI 接入（config.toml）：</Text>
                  <pre style={preStyle}>
                    {codexExample}
                  </pre>
                </div>
              </div>
            ),
          },
        ]}
      />
    </Modal>
  );
};
