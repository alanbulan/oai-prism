import React, { useEffect, useRef, useState } from 'react';
import { Alert, Modal, Tabs, Input, Upload, message, Typography, Space, Button } from 'antd';
import {
  InboxOutlined,
  KeyOutlined,
  FileTextOutlined,
  SafetyCertificateOutlined,
  LinkOutlined,
} from '@ant-design/icons';
import { httpClient } from '../../../infrastructure/http/client';
import { useAccountStore } from '../../../application/account/store';

const { TextArea } = Input;
const { Dragger } = Upload;
const { Text } = Typography;

/** 官方 OAuth 授权导入：浏览器走 auth.openai.com 官方授权页，本地回调接 code 换 token。 */
const OAuthImportPane: React.FC<{ onDone: () => void }> = ({ onDone }) => {
  const [phase, setPhase] = useState<'idle' | 'waiting' | 'success' | 'error'>('idle');
  const [sessionId, setSessionId] = useState('');
  const [errMsg, setErrMsg] = useState('');
  const [callbackUrl, setCallbackUrl] = useState('');
  const timerRef = useRef<number | null>(null);

  // 离开弹窗/组件卸载时停掉轮询
  useEffect(() => {
    return () => {
      if (timerRef.current) window.clearInterval(timerRef.current);
    };
  }, []);

  const begin = async () => {
    try {
      const res = await httpClient.post<any>('/admin/oauth/begin', {
        redirect_uri: 'http://localhost:1455/auth/callback',
      });
      const { session_id, authorize_url } = res.data || {};
      if (!session_id || !authorize_url) {
        message.error('后端未返回授权地址');
        return;
      }
      setSessionId(session_id);
      setPhase('waiting');
      window.open(authorize_url, '_blank', 'noopener');

      // 轮询导入进度（回调监听器自动完成时结束）
      timerRef.current = window.setInterval(async () => {
        try {
          const st = await httpClient.get<any>(`/admin/oauth/status`, {
            params: { session_id },
          });
          const status = st.data?.status;
          if (status === 'success') {
            window.clearInterval(timerRef.current!);
            setPhase('success');
            message.success(`授权完成，账号已入库（${st.data.account_id}）`);
            onDone();
          } else if (status === 'error') {
            window.clearInterval(timerRef.current!);
            setErrMsg(st.data?.error || '未知错误');
            setPhase('error');
          }
        } catch (err: any) {
          // 会话查不到（网关重启过或已过期）：再等也不会有结果，停下来说明原因；其它错误下个周期重试
          if (err?.response?.status === 404) {
            window.clearInterval(timerRef.current!);
            setErrMsg('授权会话已失效（网关重启过或超过 30 分钟），请重新点击“打开官方授权页”');
            setPhase('error');
          }
        }
      }, 2000);
    } catch (err: any) {
      message.error(err.message || '发起授权失败');
    }
  };

  const exchangeManual = async () => {
    if (!callbackUrl.trim()) {
      message.warning('请粘贴授权后浏览器跳转的完整回调地址');
      return;
    }
    try {
      const res = await httpClient.post<any>('/admin/oauth/exchange', {
        session_id: sessionId,
        callback: callbackUrl.trim(),
      });
      message.success(`授权完成，账号已入库（${res.data?.account_id}）`);
      setPhase('success');
      onDone();
    } catch (err: any) {
      message.error(err.response?.data?.error || err.message || '换 token 失败');
    }
  };

  if (phase === 'success') {
    return (
      <Alert
        type="success"
        showIcon
        message="授权完成"
        description="账号已写入 SQLite 并进入调度池，可在列表中查看。"
      />
    );
  }

  return (
    <Space orientation="vertical" style={{ width: '100%' }} size="middle">
      <Text type="secondary">
        跳转到 auth.openai.com 官方授权页登录（与 Codex CLI 同款 PKCE 流程），
        授权完成后本地回调端口自动换取 token 并入库，全程无需手动复制凭据。
      </Text>

      {phase === 'waiting' && (
        <Alert
          type="info"
          showIcon
          message="等待授权完成…"
          description="已在浏览器打开官方授权页。若本地回调端口（1455）被占用，可把授权完成后浏览器地址栏里的完整回调地址粘贴到下方手动完成导入。"
        />
      )}

      {phase === 'error' && (
        <Alert type="error" showIcon message="导入失败" description={errMsg} />
      )}

      <div>
        <Button type="primary" icon={<SafetyCertificateOutlined />} onClick={begin} loading={phase === 'waiting' && !errMsg}>
          打开官方授权页
        </Button>
      </div>

      {(phase === 'waiting' || phase === 'error') && (
        <div>
          <Text type="secondary" style={{ fontSize: 12 }}>
            手动兜底：粘贴授权后浏览器跳转的完整地址（含 code 参数）：
          </Text>
          <Space.Compact style={{ width: '100%', marginTop: 6 }}>
            <Input
              prefix={<LinkOutlined />}
              placeholder="http://localhost:1455/auth/callback?code=...&state=..."
              value={callbackUrl}
              onChange={(e) => setCallbackUrl(e.target.value)}
            />
            <Button onClick={exchangeManual}>完成导入</Button>
          </Space.Compact>
        </div>
      )}
    </Space>
  );
};


export const AccountImportModal: React.FC = () => {
  const { importModalOpen, setImportModalOpen, importAccounts, fetchAccounts } = useAccountStore();
  const [activeTab, setActiveTab] = useState('text');
  const [rawText, setRawText] = useState('');
  const [loading, setLoading] = useState(false);

  const submit = async (text: string, source?: string) => {
    setLoading(true);
    try {
      await importAccounts({ rawText: text });
      message.success(source ? `已导入 ${source}` : '账号导入成功！');
      setRawText('');
      setImportModalOpen(false);
    } catch (err: any) {
      const reason = err.message || '导入失败，请检查数据格式';
      if (source) {
        // 文件导入失败：内容放进文本页，方便看清哪里不对、改完再导
        setRawText(text);
        setActiveTab('text');
        message.error(`${source} 导入失败：${reason}（内容已放到文本页）`);
      } else {
        message.error(reason);
      }
    } finally {
      setLoading(false);
    }
  };

  const handleOk = async () => {
    if (!rawText.trim()) {
      message.warning('请先输入或上传账号凭据内容');
      return;
    }
    await submit(rawText);
  };

  const handleOAuthDone = () => {
    fetchAccounts();
    setImportModalOpen(false);
  };

  const items = [
    {
      key: 'oauth',
      label: (
        <span>
          <SafetyCertificateOutlined /> 官方授权登录
        </span>
      ),
      children: <OAuthImportPane onDone={handleOAuthDone} />,
    },
    {
      key: 'text',
      label: (
        <span>
          <KeyOutlined /> 文本 / JSON 粘贴
        </span>
      ),
      children: (
        <Space orientation="vertical" style={{ width: '100%' }}>
          <Text type="secondary">
            支持单行/多行 Cookie 字符串，或直接粘贴账号 JSON（accounts.json、其它网关如 sub2api 的导出文件均可）：
          </Text>
          <TextArea
            rows={8}
            placeholder={'__Secure-next-auth.session-token=eyJhbGciOiJkaXIi...\n或\n[{"name":"Team通道","cookie":"...","plan":"Enterprise"}]'}
            value={rawText}
            onChange={(e) => setRawText(e.target.value)}
          />
        </Space>
      ),
    },
    {
      key: 'file',
      label: (
        <span>
          <FileTextOutlined /> 文件拖拽导入
        </span>
      ),
      children: (
        <Dragger
          accept=".json,.txt"
          showUploadList={false}
          disabled={loading}
          beforeUpload={(file) => {
            // 拖进来就直接导入；失败时才把内容放进文本页
            file
              .text()
              .then((content) => {
                if (!content.trim()) {
                  message.warning(`${file.name} 是空文件`);
                  return;
                }
                return submit(content, file.name);
              })
              .catch((err) => message.error(`读取 ${file.name} 失败：${err?.message || err}`));
            return false;
          }}
        >
          <p className="ant-upload-drag-icon">
            <InboxOutlined />
          </p>
          <p className="ant-upload-text">点击或拖拽 accounts.json 或 cookie.txt 文件到此区域，放下即导入</p>
          <p className="ant-upload-hint">支持单个或批量账号 JSON，包括其它网关（如 sub2api）导出的账号文件</p>
        </Dragger>
      ),
    },
  ];

  return (
    <Modal
      title="导入账号凭据"
      open={importModalOpen}
      onOk={handleOk}
      confirmLoading={loading}
      onCancel={() => setImportModalOpen(false)}
      width={600}
      okText="确认导入"
      cancelText="取消"
      okButtonProps={activeTab === 'text' ? undefined : { style: { display: 'none' } }}
    >
      <Tabs activeKey={activeTab} onChange={setActiveTab} items={items} />
    </Modal>
  );
};
