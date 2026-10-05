import React, { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { Avatar, Button, Card, Dropdown, Input, List, Modal, Pagination, Popconfirm, Space, Tooltip, Typography, Upload, message, theme } from 'antd';
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
  CopyOutlined,
} from '@ant-design/icons';
import { Bubble, Sender, ThoughtChain, Prompts } from '@ant-design/x';
import type { ReasoningEffort } from '../../../domain/chat/entity';
import { effortsForModel } from '../../../domain/modelFilter';
import { useChatStore } from '../../../application/chat/store';
import { BrandLogo } from '../../components/BrandLogo';
import { SPECTRUM } from '../../theme/tokens';
import { MarkdownView } from './markdown/MarkdownView';
import { ContextMeter } from './ContextMeter';
import { MessageMeta } from './MessageMeta';

const { Text } = Typography;

/** 推理强度档位的展示名（顺序 = 低/中/高/极高） */
const EFFORT_LABELS: Record<ReasoningEffort, string> = {
  low: '低 (Low)',
  medium: '中 (Medium)',
  high: '高 (High)',
  xhigh: '极高 (xHigh)',
};
const EFFORT_ORDER: ReasoningEffort[] = ['low', 'medium', 'high', 'xhigh'];

/** 会话列表一行的高度（含行间距）与分页条高度：每页条数按列表区实际高度算，不留大片空白 */
const CONV_ROW = 34;
const CONV_PAGER = 40;


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
  const [convNav, setConvNav] = useState<{ page: number; anchor: string | null; size: number } | null>(null);
  const convBoxRef = useRef<HTMLDivElement>(null);
  const [convBoxHeight, setConvBoxHeight] = useState(0);
  const [renaming, setRenaming] = useState<{ id: string; title: string } | null>(null);

  useLayoutEffect(() => {
    const el = convBoxRef.current;
    if (!el) return;
    const fit = () => setConvBoxHeight(el.clientHeight);
    fit();
    const ro = new ResizeObserver(fit);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  // 一页放得下就不分页；放不下时给分页条让出一行的位置再算每页条数
  const convRowsAll = Math.max(1, Math.floor(convBoxHeight / CONV_ROW));
  const convPaged = sessions.length > convRowsAll;
  const convPageSize = convPaged ? Math.max(3, Math.floor((convBoxHeight - CONV_PAGER) / CONV_ROW)) : convRowsAll;

  useEffect(() => {
    init();
  }, [init]);

  // 当前模型的可用推理档位（由后端清单中的档位变体推导，如 6 Luna 没有 low）
  const availableEfforts = effortsForModel(selectedModel, allModelIds);

  // 消息区自动滚底：切换会话时直接到底；流式增量只在用户本就停在底部时跟随，
  // 往上翻阅历史时不被新内容拽回去。
  const stickToBottom = useRef(true);
  const onMessagesScroll = () => {
    const el = msgListRef.current;
    if (el) stickToBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
  };
  useEffect(() => {
    stickToBottom.current = true;
  }, [currentSessionId]);
  useEffect(() => {
    const el = msgListRef.current;
    if (el && stickToBottom.current) el.scrollTop = el.scrollHeight;
  }, [sessions, currentSessionId]);

  const copyMessage = async (text: string) => {
    try {
      await navigator.clipboard.writeText(text);
      message.success('已复制整条回复（Markdown 原文）');
    } catch {
      message.error('复制失败：浏览器未授权剪贴板');
    }
  };

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

  // 默认停在当前会话所在的那一页；手动翻页只在"同一个当前会话、同样的每页条数"下有效 ——
  // 切换 / 新建 / 删除会话或窗口高度变了，就回到当前会话所在页
  const convPageCount = Math.max(1, Math.ceil(sessions.length / convPageSize));
  const activeIdx = sessions.findIndex((s) => s.id === currentSessionId);
  const followPage = activeIdx >= 0 ? Math.floor(activeIdx / convPageSize) + 1 : 1;
  const convPage =
    convNav && convNav.anchor === currentSessionId && convNav.size === convPageSize ? convNav.page : followPage;
  const safeConvPage = Math.min(convPage, convPageCount);
  const convPageItems = sessions.slice((safeConvPage - 1) * convPageSize, safeConvPage * convPageSize);

  // Bubble 列表转换
  const bubbleItems = messages.map((m) => {
    const isUser = m.role === 'user';
    const streaming = m.status === 'loading';
    return {
      key: m.id,
      role: m.role,
      placement: (isUser ? 'end' : 'start') as 'end' | 'start',
      // 用户消息：浅色气泡；助手回复：无边框、撑满消息区（代码与 HTML 预览需要整宽）
      variant: (isUser ? 'filled' : 'borderless') as 'filled' | 'borderless',
      styles: isUser
        ? { content: { background: token.colorPrimaryBg, borderRadius: 14, maxWidth: 720 } }
        : {
            // 组件默认给左侧气泡右边留 15% 空白，助手回复要整宽
            root: { paddingInlineEnd: 0 },
            body: { flex: 1, minWidth: 0 },
            content: { width: '100%', padding: '4px 0 0' },
          },
      // 底部一行：发送 / 回复时间、本轮耗时与用量（回复完成后带复制按钮）
      footerPlacement: (isUser ? 'outer-end' : 'outer-start') as 'outer-end' | 'outer-start',
      footer: (
        <MessageMeta m={m}>
          {!isUser && !streaming && m.content && (
            <Tooltip title="复制整条回复">
              <Button
                type="text"
                size="small"
                icon={<CopyOutlined />}
                onClick={() => copyMessage(m.content)}
                style={{ marginInlineStart: -6 }}
              />
            </Tooltip>
          )}
        </MessageMeta>
      ),
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
                    description: <MarkdownView content={m.reasoning} streaming={streaming} className="md-reasoning" />,
                  },
                ]}
              />
            </div>
          )}
          {isUser ? (
            <div style={{ whiteSpace: 'pre-wrap', fontSize: 14 }}>{m.content}</div>
          ) : (
            m.content ? (
              <MarkdownView content={m.content} streaming={streaming} />
            ) : streaming ? (
              <Text type="secondary">正在思考生成中…</Text>
            ) : null
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
          {/* 列表区撑满剩余高度，每页条数按它的高度算；分页条贴在底部 */}
          <div ref={convBoxRef} style={{ flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column' }}>
          <List
            size="small"
            split={false}
            dataSource={convPageItems}
            locale={{ emptyText: '暂无会话' }}
            style={{ flex: 1, minHeight: 0, overflow: 'hidden' }}
            renderItem={(s) => (
              <List.Item
                className="conv-item"
                onClick={() => selectSession(s.id)}
                style={{
                  cursor: 'pointer',
                  height: CONV_ROW - 2,
                  padding: '0 10px',
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
                  <Tooltip title={s.title} placement="right" mouseEnterDelay={0.6}>
                    <span style={{ display: 'block', fontSize: 13, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                      {s.title}
                    </span>
                  </Tooltip>
                </div>
              </List.Item>
            )}
          />
          {convPaged && (
            <div style={{ height: CONV_PAGER, flexShrink: 0, display: 'flex', alignItems: 'center', justifyContent: 'center', borderTop: `1px solid ${token.colorBorderSecondary}` }}>
              <Pagination
                size="small"
                simple={convPageCount > 5}
                showLessItems
                showSizeChanger={false}
                current={safeConvPage}
                pageSize={convPageSize}
                total={sessions.length}
                onChange={(pg) => setConvNav({ page: pg, anchor: currentSessionId, size: convPageSize })}
              />
            </div>
          )}
          </div>
        </div>

        {/* 右侧对话主体区（官方 Bubble.List + Sender） */}
        <div style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' }}>
          {/* 消息展示区：撑满剩余高度、内部滚动、自动跟随到底 */}
          <div ref={msgListRef} onScroll={onMessagesScroll} style={{ flex: 1, minHeight: 0, padding: '20px 24px', overflowY: 'auto' }}>
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

          {/* 底部输入框（官方 Sender）：模型/强度切换与附件按钮都在输入框内，ChatGPT 式交互 */}
          <div style={{ padding: '12px 20px 16px', borderTop: `1px solid ${token.colorBorderSecondary}` }}>
            <Sender
              value={input}
              onChange={setInput}
              onSubmit={handleSend}
              loading={isStreaming}
              placeholder="输入调试指令，例如：生成一个鹈鹕骑自行车的 SVG，用 HTML 实现..."
              suffix={(ori) => (
                <Space size={4} align="center">
                  <ContextMeter messages={messages} />
                  {ori}
                </Space>
              )}
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
