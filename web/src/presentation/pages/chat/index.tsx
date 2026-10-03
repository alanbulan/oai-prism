import React, { useEffect, useMemo, useRef, useState } from 'react';
import { Avatar, Button, Card, Dropdown, Input, List, Modal, Popconfirm, Space, Typography, Upload, message, theme } from 'antd';
import {
  RobotOutlined,
  UserOutlined,
  PlusOutlined,
  ThunderboltOutlined,
  BulbOutlined,
  CodeOutlined,
  FileSearchOutlined,
  PictureOutlined,
  DownOutlined,
  CheckOutlined,
  PaperClipOutlined,
  CloseOutlined,
  EditOutlined,
  DeleteOutlined,
  LinkOutlined,
} from '@ant-design/icons';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import rehypeRaw from 'rehype-raw';
import { Bubble, Sender, ThoughtChain, Prompts } from '@ant-design/x';
import type { ReasoningEffort } from '../../../domain/chat/entity';
import { effortsForModel } from '../../../domain/modelFilter';
import { useChatStore } from '../../../application/chat/store';
import { BrandLogo } from '../../components/BrandLogo';
import { SPECTRUM } from '../../theme/tokens';

const { Text } = Typography;

/** 推理强度档位的展示名（顺序 = 低/中/高/极高） */
const EFFORT_LABELS: Record<ReasoningEffort, string> = {
  low: '低 (Low)',
  medium: '中 (Medium)',
  high: '高 (High)',
  xhigh: '极高 (xHigh)',
};
const EFFORT_ORDER: ReasoningEffort[] = ['low', 'medium', 'high', 'xhigh'];


/** HTML/SVG 代码块的渲染预览：iframe 直出 + 源码切换 + 新窗口打开 */
const HtmlPreview: React.FC<{ code: string; lang: string }> = ({ code, lang }) => {
  const { token } = theme.useToken();
  const [showSource, setShowSource] = useState(false);
  const blobUrl = useMemo(() => URL.createObjectURL(new Blob([code], { type: 'text/html' })), [code]);
  useEffect(() => () => URL.revokeObjectURL(blobUrl), [blobUrl]);

  return (
    <div style={{ border: `1px solid ${token.colorBorderSecondary}`, borderRadius: 10, overflow: 'hidden', margin: '8px 0', background: token.colorBgContainer }}>
      <div style={{ background: token.colorFillQuaternary, padding: '4px 10px', display: 'flex', justifyContent: 'space-between', alignItems: 'center', borderBottom: `1px solid ${token.colorBorderSecondary}` }}>
        <span style={{ fontSize: 12, color: token.colorTextSecondary }}>{lang === 'svg' ? 'SVG 渲染预览' : 'HTML 渲染预览'}</span>
        <Space size={0}>
          <Button type="text" size="small" onClick={() => setShowSource(!showSource)}>
            {showSource ? '渲染结果' : '查看源码'}
          </Button>
          <Button type="text" size="small" icon={<LinkOutlined />} onClick={() => window.open(blobUrl, '_blank')} title="在新窗口打开" />
        </Space>
      </div>
      {showSource ? (
        <pre style={{ background: 'var(--op-code-bg)', margin: 0, padding: '10px 12px', overflowX: 'auto', fontSize: 12 }}>
          <code>{code}</code>
        </pre>
      ) : (
        <iframe
          srcDoc={code}
          title="html-preview"
          sandbox="allow-scripts"
          style={{ width: '100%', height: 340, border: 'none', background: '#fff', display: 'block' }}
        />
      )}
    </div>
  );
};

export const ChatPlaygroundPage: React.FC = () => {
  const { token } = theme.useToken();
  const {
    models,
    allModelIds,
    sessions,
    currentSessionId,
    selectedModel,
    reasoningEffort,
    isStreaming,
    lastUsage,
    init,
    selectSession,
    createNewSession,
    deleteSession,
    renameSession,
    setModel,
    setReasoningEffort,
    sendMessage,
  } = useChatStore();

  const [input, setInput] = useState('');
  const [attachments, setAttachments] = useState<{ name: string; dataUrl: string }[]>([]);
  const msgListRef = useRef<HTMLDivElement>(null);

  // 会话列表分页 + 重命名
  const [convPage, setConvPage] = useState(1);
  const convPageSize = 8;
  const [renaming, setRenaming] = useState<{ id: string; title: string } | null>(null);

  useEffect(() => {
    init();
  }, [init]);

  // 当前模型的可用推理档位（由后端清单中的档位变体推导，如 6 Luna 没有 low）
  const availableEfforts = effortsForModel(selectedModel, allModelIds);

  // 消息区自动滚底（流式增量与首屏渲染都跟随）
  useEffect(() => {
    const el = msgListRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [sessions, currentSessionId]);

  const activeSession = sessions.find((s) => s.id === currentSessionId);
  const messages = activeSession?.messages || [];
  const currentModel = models.find((m) => m.id === selectedModel);
  const effortLabel = EFFORT_LABELS[reasoningEffort];

  const handleSend = () => {
    if (!input.trim() || isStreaming) return;
    const text = input;
    const atts = attachments;
    setInput('');
    setAttachments([]);
    sendMessage(text, atts.length > 0 ? atts : undefined);
  };

  // 附件：本地读取为 base64 data URL，随消息走 OpenAI image_url 多模态格式
  const handleAttach = (file: File) => {
    if (attachments.length >= 4) {
      message.warning('最多附加 4 张图片');
      return false;
    }
    const reader = new FileReader();
    reader.onload = () =>
      setAttachments((prev) => [...prev, { name: file.name, dataUrl: String(reader.result) }]);
    reader.readAsDataURL(file);
    return false; // 手动处理，不触发 antd 默认上传
  };

  // 推荐提示词项
  const promptItems = [
    {
      key: 'p1',
      icon: <CodeOutlined style={{ color: SPECTRUM[0] }} />,
      description: '生成一个鹈鹕骑自行车的 SVG，用 HTML 实现',
    },
    {
      key: 'p2',
      icon: <FileSearchOutlined style={{ color: SPECTRUM[2] }} />,
      description: '测试本地文件修改，查看 Unified Diff 工具调用',
    },
    {
      key: 'p3',
      icon: <PictureOutlined style={{ color: SPECTRUM[3] }} />,
      description: '上传并分析图片附件，测试多模态输入能力',
    },
  ];

  // 会话列表 dataSource（List 组件自带分页切片）
  const convPageCount = Math.max(1, Math.ceil(sessions.length / convPageSize));
  const safeConvPage = Math.min(convPage, convPageCount);

  // Bubble 列表转换
  const bubbleItems = messages.map((m) => {
    const isUser = m.role === 'user';
    return {
      key: m.id,
      role: m.role,
      placement: (isUser ? 'end' : 'start') as 'end' | 'start',
      avatar: isUser ? (
        <Avatar icon={<UserOutlined />} style={{ background: `linear-gradient(135deg, ${token.colorPrimary}, #a855f7)` }} />
      ) : (
        <BrandLogo size={32} />
      ),
      content: (
        <div>
          {/* 用户消息附带的图片附件（多模态输入回显） */}
          {m.attachments && m.attachments.length > 0 && (
            <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginBottom: m.content ? 8 : 0 }}>
              {m.attachments.map((a, i) => (
                <img
                  key={i}
                  src={a.dataUrl}
                  alt={a.name}
                  style={{ maxWidth: 200, maxHeight: 150, borderRadius: 8, border: `1px solid ${token.colorBorderSecondary}`, objectFit: 'cover' }}
                />
              ))}
            </div>
          )}
          {/* 若包含思考链，使用 @ant-design/x 的 ThoughtChain 组件呈现 */}
          {m.reasoning && (
            <div style={{ marginBottom: 8 }}>
              <ThoughtChain
                items={[
                  {
                    title: '深度推理过程',
                    status: m.status === 'loading' ? 'loading' : 'success',
                    description: (
                      <Text type="secondary" style={{ whiteSpace: 'pre-wrap', fontSize: 13 }}>
                        {m.reasoning}
                      </Text>
                    ),
                  },
                ]}
              />
            </div>
          )}
          {isUser ? (
            <div style={{ whiteSpace: 'pre-wrap', fontSize: 14 }}>{m.content}</div>
          ) : (
            <div className="md-body">
              {m.content ? (
                <ReactMarkdown
                  remarkPlugins={[remarkGfm]}
                  rehypePlugins={[rehypeRaw]}
                  components={{
                    // html/svg 代码块 → 渲染预览（iframe 直出），其余走默认
                    pre: ({ children }: any) => {
                      const child: any = Array.isArray(children) ? children[0] : children;
                      const cls: string = child?.props?.className || '';
                      const lang = (/language-(\w+)/.exec(cls)?.[1] || '').toLowerCase();
                      const raw = String(child?.props?.children ?? '').replace(/\n$/, '');
                      if ((lang === 'html' || lang === 'svg') && raw) {
                        return <HtmlPreview code={raw} lang={lang} />;
                      }
                      return <pre>{children}</pre>;
                    },
                  }}
                >
                  {m.content}
                </ReactMarkdown>
              ) : m.status === 'loading' ? (
                '正在思考生成中...'
              ) : null}
            </div>
          )}
        </div>
      ),
      loading: m.status === 'loading' && !m.content && !m.reasoning,
    };
  });

  // 模型切换下拉（清单来自后端 /v1/models，无前端硬编码）
  const modelMenu = {
    items: models.map((m) => ({
      key: m.id,
      label: (
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 16, minWidth: 160 }}>
          <span>{m.name}</span>
          {m.id === selectedModel && <CheckOutlined style={{ color: token.colorPrimary }} />}
        </div>
      ),
    })),
    selectedKeys: [selectedModel],
    onClick: ({ key }: { key: string }) => setModel(key),
  };

  // 推理强度下拉（选项随模型动态变化：各模型的档位由后端配置决定）
  const effortMenu = {
    items: EFFORT_ORDER.filter((e) => availableEfforts.includes(e)).map((e) => ({
      key: e,
      label: (
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 16, minWidth: 120 }}>
          <span>{EFFORT_LABELS[e]}</span>
          {e === reasoningEffort && <CheckOutlined style={{ color: token.colorPrimary }} />}
        </div>
      ),
    })),
    selectedKeys: [reasoningEffort],
    onClick: ({ key }: { key: string }) => setReasoningEffort(key as ReasoningEffort),
  };

  return (
    <Card
      styles={{
        // 卡片撑满 Content 容器；body 为纵向 flex：会话区撑满、输入区贴底
        body: { padding: 0, flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column' },
      }}
      className="page-fill"
      style={{ flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column', width: '100%', boxShadow: token.boxShadowTertiary }}
      title={
        <Space size="small">
          <RobotOutlined style={{ color: token.colorPrimary }} />
          <span style={{ fontWeight: 600 }}>调试控制台</span>
        </Space>
      }
      extra={
        <Button type="primary" icon={<PlusOutlined />} onClick={createNewSession}>
          新建会话
        </Button>
      }
    >
      <div style={{ flex: 1, minHeight: 0, display: 'flex' }}>
        {/* 左侧会话列表：标准 CRUD（重命名/删除菜单）+ 分页 */}
        <div
          style={{
            width: 240,
            flexShrink: 0,
            borderRight: `1px solid ${token.colorBorderSecondary}`,
            padding: 12,
            display: 'flex',
            flexDirection: 'column',
            minHeight: 0,
          }}
        >
          <div style={{ marginBottom: 12, fontWeight: 600, color: token.colorTextSecondary, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
            <span><BulbOutlined /> 会话列表</span>
            <Text type="secondary" style={{ fontSize: 12 }}>共 {sessions.length} 个</Text>
          </div>
          <List
            size="small"
            dataSource={sessions}
            pagination={
              sessions.length > convPageSize
                ? { pageSize: convPageSize, size: 'small', current: safeConvPage, total: sessions.length, onChange: (pg) => setConvPage(pg), style: { marginBottom: 0 } }
                : false
            }
            locale={{ emptyText: '暂无会话' }}
            renderItem={(s) => (
              <List.Item
                onClick={() => selectSession(s.id)}
                style={{
                  cursor: 'pointer',
                  padding: '6px 10px',
                  borderRadius: 6,
                  marginBottom: 2,
                  background: s.id === currentSessionId ? token.colorPrimaryBg : 'transparent',
                }}
                actions={[
                  <Button
                    key="rename"
                    type="text"
                    size="small"
                    icon={<EditOutlined />}
                    onClick={(e) => {
                      e.stopPropagation();
                      setRenaming({ id: s.id, title: s.title });
                    }}
                  />,
                  <Popconfirm
                    key="delete"
                    title="删除该会话？"
                    okText="删除"
                    cancelText="取消"
                    okButtonProps={{ danger: true }}
                    onConfirm={(e) => {
                      e?.stopPropagation();
                      deleteSession(s.id);
                    }}
                  >
                    <Button
                      type="text"
                      size="small"
                      danger
                      icon={<DeleteOutlined />}
                      onClick={(e) => e.stopPropagation()}
                    />
                  </Popconfirm>,
                ]}
              >
                {/* 单行强制：flex + minWidth:0 + ellipsis，任何长度都不折行 */}
                <div style={{ flex: 1, minWidth: 0, overflow: 'hidden' }}>
                  <span style={{ display: 'block', fontSize: 13, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                    {s.title}
                  </span>
                </div>
              </List.Item>
            )}
          />
        </div>

        {/* 右侧对话主体区（官方 Bubble.List + Sender） */}
        <div style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' }}>
          {/* 消息展示区：撑满剩余高度、内部滚动、自动跟随到底 */}
          <div ref={msgListRef} style={{ flex: 1, minHeight: 0, padding: 20, overflowY: 'auto' }}>
            {messages.length === 0 ? (
              <div style={{ textAlign: 'center', marginTop: 60 }}>
                <BrandLogo size={56} style={{ margin: '0 auto' }} />
                <h3 style={{ marginTop: 16, color: token.colorTextHeading }}>欢迎体验 OAIprism 交互式调试终端</h3>
                <p style={{ color: token.colorTextSecondary, maxWidth: 500, margin: '0 auto' }}>
                  直连上游 Prism 代理，模型清单实时来自后端 /v1/models，支持工具调用落盘测试、多模态图片输入与滑动窗口压缩。
                </p>
                <div style={{ marginTop: 24, display: 'inline-block', textAlign: 'left' }}>
                  <Prompts
                    title="推荐快捷调试场景："
                    items={promptItems}
                    onItemClick={(item) => {
                      if (item.data?.description) {
                        setInput(String(item.data.description));
                      }
                    }}
                  />
                </div>
              </div>
            ) : (
              <Bubble.List items={bubbleItems} />
            )}
          </div>

          {/* 上下文窗口指示：本轮 prompt ≈ 当前会话累计上下文占用（网关估算） */}
          {lastUsage && (
            <div style={{ padding: '6px 20px 0', display: 'flex', justifyContent: 'flex-end' }}>
              <Text type="secondary" style={{ fontSize: 11 }}>
                上下文窗口 ≈ {lastUsage.promptTokens.toLocaleString()} tokens（本轮输出 {lastUsage.completionTokens}）
              </Text>
            </div>
          )}

          {/* 底部输入框（官方 Sender）：模型/强度切换与附件按钮都在输入框内，ChatGPT 式交互 */}
          <div style={{ padding: '12px 20px 16px', borderTop: `1px solid ${token.colorBorderSecondary}` }}>
            <Sender
              value={input}
              onChange={setInput}
              onSubmit={handleSend}
              loading={isStreaming}
              placeholder="输入调试指令，例如：生成一个鹈鹕骑自行车的 SVG，用 HTML 实现..."
              header={
                attachments.length > 0 ? (
                  <div style={{ padding: '10px 12px 0', display: 'flex', gap: 10, flexWrap: 'wrap' }}>
                    {attachments.map((a, i) => (
                      <div key={i} style={{ position: 'relative' }}>
                        <img
                          src={a.dataUrl}
                          alt={a.name}
                          style={{ width: 56, height: 56, objectFit: 'cover', borderRadius: 8, border: `1px solid ${token.colorBorderSecondary}`, display: 'block' }}
                        />
                        <Button
                          size="small"
                          shape="circle"
                          icon={<CloseOutlined style={{ fontSize: 10 }} />}
                          style={{ position: 'absolute', top: -6, right: -6, width: 18, height: 18, minWidth: 18 }}
                          onClick={() => setAttachments((prev) => prev.filter((_, j) => j !== i))}
                        />
                      </div>
                    ))}
                  </div>
                ) : undefined
              }
              prefix={
                <Space size={2} wrap>
                  <Dropdown menu={modelMenu} trigger={['click']} placement="topLeft">
                    <Button type="text" shape="round" icon={<RobotOutlined style={{ color: token.colorPrimary }} />}>
                      {currentModel?.name || selectedModel}
                      <DownOutlined style={{ fontSize: 10, color: token.colorTextTertiary }} />
                    </Button>
                  </Dropdown>
                  <Dropdown menu={effortMenu} trigger={['click']} placement="topLeft">
                    <Button type="text" shape="round" icon={<ThunderboltOutlined style={{ color: token.colorWarning }} />}>
                      {effortLabel}
                      <DownOutlined style={{ fontSize: 10, color: token.colorTextTertiary }} />
                    </Button>
                  </Dropdown>
                  <Upload accept="image/*" showUploadList={false} beforeUpload={handleAttach}>
                    <Button type="text" shape="round" icon={<PaperClipOutlined style={{ color: token.colorPrimary }} />} title="附加图片（多模态输入）" />
                  </Upload>
                </Space>
              }
            />
          </div>
        </div>
      </div>

      <Modal
        title="重命名会话"
        open={Boolean(renaming)}
        onOk={() => {
          if (renaming) renameSession(renaming.id, renaming.title);
          setRenaming(null);
        }}
        onCancel={() => setRenaming(null)}
        okText="保存"
        cancelText="取消"
        width={420}
        destroyOnHidden
      >
        <Input
          value={renaming?.title || ''}
          onChange={(e) => setRenaming((prev) => (prev ? { ...prev, title: e.target.value } : prev))}
          placeholder="会话名称"
          autoFocus
        />
      </Modal>
    </Card>
  );
};
