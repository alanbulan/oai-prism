import type {
  ChatAttachment,
  ChatModelInfo,
  ChatMessage,
  ChatMessageMeta,
  ChatSession,
  ChatSessionPrefs,
  IChatRepository,
  SendMessageOptions,
} from '../../domain/chat/entity';
import { compareModelNewestFirst, pickMainModels } from '../../domain/modelFilter';
import { getApiKey, httpClient } from '../http/client';

// 浏览器本地存储：会话的模型 / 推理强度偏好、回复的耗时与用量、上次打开的会话。
// 服务端只存会话与消息正文；这些是调试台自己的展示状态，不值得改库表。
const PREFS_KEY = 'oaiprism_chat_prefs';
const META_KEY = 'oaiprism_chat_meta';
const ACTIVE_KEY = 'oaiprism_chat_active';

function readJSON<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(key);
    return raw ? (JSON.parse(raw) as T) : fallback;
  } catch {
    return fallback;
  }
}

function writeJSON(key: string, value: unknown): void {
  try {
    localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // 存储已满或被禁用：只影响本地展示，忽略
  }
}

type MetaStore = Record<string, Record<string, ChatMessageMeta>>; // 会话 ID → 消息 ID → 附加信息

export class ChatRepositoryImpl implements IChatRepository {
  loadSessionPrefs(id: string): ChatSessionPrefs | null {
    return readJSON<Record<string, ChatSessionPrefs>>(PREFS_KEY, {})[id] ?? null;
  }

  saveSessionPrefs(id: string, prefs: ChatSessionPrefs): void {
    const all = readJSON<Record<string, ChatSessionPrefs>>(PREFS_KEY, {});
    all[id] = prefs;
    writeJSON(PREFS_KEY, all);
  }

  saveMessageMeta(sessionId: string, msgId: string, meta: ChatMessageMeta): void {
    const all = readJSON<MetaStore>(META_KEY, {});
    all[sessionId] = { ...all[sessionId], [msgId]: meta };
    writeJSON(META_KEY, all);
  }

  loadActiveSessionId(): string | null {
    return readJSON<string | null>(ACTIVE_KEY, null);
  }

  saveActiveSessionId(id: string | null): void {
    writeJSON(ACTIVE_KEY, id);
  }

  private dropLocal(sessionId: string): void {
    const prefs = readJSON<Record<string, ChatSessionPrefs>>(PREFS_KEY, {});
    const meta = readJSON<MetaStore>(META_KEY, {});
    delete prefs[sessionId];
    delete meta[sessionId];
    writeJSON(PREFS_KEY, prefs);
    writeJSON(META_KEY, meta);
  }

  async fetchModelCatalog(): Promise<{ mains: ChatModelInfo[]; allIds: string[] }> {
    try {
      const res = await httpClient.get<any>('/v1/models');
      const data = res.data?.data || [];
      if (Array.isArray(data) && data.length > 0) {
        // 后端 /v1/models 已只暴露现役模型（含各自主模型的档位变体）。
        // mains = 不带档位后缀的条目；allIds 全量保留 —— 各模型的可用推理档位
        // 由其 effort 变体是否存在推导（domain/modelFilter.ts）。
        const mains = pickMainModels(data).sort(compareModelNewestFirst);
        if (mains.length > 0) {
          return {
            mains: mains.map((m: any) => ({ id: m.id, name: m.name || m.id })),
            allIds: data.map((m: any) => m.id),
          };
        }
      }
    } catch {
      // 容灾兜底：后端不可达时仅保旗舰，避免空 UI
    }
    return {
      mains: [{ id: 'gpt-6.1-sol', name: '6.1 Sol' }],
      allIds: ['gpt-6.1-sol', 'gpt-6.1-sol-low', 'gpt-6.1-sol-high', 'gpt-6.1-sol-xhigh'],
    };
  }

  async listSessions(): Promise<ChatSession[]> {
    try {
      const res = await httpClient.get<any[]>('/admin/chat/sessions');
      const list = res.data || [];

      if (list.length === 0) {
        // 后端无会话记录，在 SQLite 初始化默认引导会话
        const defaultSession: ChatSession = {
          id: 'sess_default_playground',
          title: '6.1 Sol 调试会话',
          model: 'gpt-6.1-sol',
          reasoningEffort: 'medium',
          createdAt: new Date().toISOString(),
          updatedAt: new Date().toISOString(),
          messages: [
            {
              id: 'msg_sys_intro',
              role: 'assistant',
              content: '你好！我是接入 OAIprism 代理网关的 Codex AI 助手（当前使用 **6.1 Sol**）。对话与调试记录已在服务端 SQLite 持久化，请随时发送测试请求！',
              createdAt: new Date().toISOString(),
              status: 'success',
            },
          ],
        };
        await this.saveSession(defaultSession);
        // 引导欢迎语单独入库（消息持久化已从 saveSession 中拆出，
        // 见 saveSession 注释 —— 否则每次完成流式都会全量重插）。
        await httpClient.post(`/admin/chat/sessions/${defaultSession.id}/messages`, {
          id: defaultSession.messages[0].id,
          role: defaultSession.messages[0].role,
          content: defaultSession.messages[0].content,
          reasoning: '',
          status: 'success',
        });
        return [defaultSession];
      }

      // 获取每个会话的消息历史（并入本地记录的耗时与用量）
      const meta = readJSON<MetaStore>(META_KEY, {});
      const sessionsWithMessages = await Promise.all(
        list.map(async (s: any): Promise<ChatSession> => {
          let msgs: ChatMessage[] = [];
          try {
            const mRes = await httpClient.get<any[]>(`/admin/chat/sessions/${s.id}/messages`);
            msgs = (mRes.data || []).map((m: any): ChatMessage => {
              // 多模态兼容：content 若为 JSON 数组串（text + image_url）→ 拆出文本与附件
              let content: string = m.content || '';
              let attachments: ChatAttachment[] | undefined;
              if (content.startsWith('[')) {
                try {
                  const parts = JSON.parse(content);
                  if (Array.isArray(parts)) {
                    content = parts
                      .filter((p: any) => p.type === 'text')
                      .map((p: any) => p.text || '')
                      .join('\n');
                    const imgs = parts
                      .filter((p: any) => p.type === 'image_url' && p.image_url?.url)
                      .map((p: any) => ({ name: '图片附件', dataUrl: p.image_url.url as string }));
                    if (imgs.length > 0) attachments = imgs;
                  }
                } catch {
                  // 非 JSON 串，按纯文本处理
                }
              }
              return {
                id: m.id,
                role: m.role,
                content,
                attachments,
                reasoning: m.reasoning || '',
                status: m.status || 'success',
                createdAt: m.created_at || new Date().toISOString(),
                ...meta[s.id]?.[m.id],
              };
            });
          } catch {
            msgs = [];
          }

          return {
            id: s.id,
            title: s.title,
            model: s.model,
            reasoningEffort: s.reasoning_effort,
            createdAt: s.created_at,
            updatedAt: s.updated_at,
            messages: msgs,
          };
        })
      );

      return sessionsWithMessages;
    } catch {
      return [];
    }
  }

  async saveSession(session: ChatSession): Promise<void> {
    try {
      // 只持久化会话元数据（新建/改名/模型切换）。
      //
      // 消息持久化由 sendMessageStream 独占负责（它有流式生命周期，
      // 分阶段写入 user / assistant）。这里如果再全量同步 messages，
      // 会在每次流式完成后的 saveSession 调用里把同样的消息再插一遍
      // —— 同一轮问答在会话里出现两组（2026-10-03 用户截图实证），
      // 前端刷新后表现为重复的问答对。
      await httpClient.post('/admin/chat/sessions', {
        id: session.id,
        title: session.title,
        model: session.model,
        reasoning_effort: session.reasoningEffort,
      });
    } catch {
      // 网络或接口异常
    }
  }

  async deleteSession(id: string): Promise<void> {
    try {
      await httpClient.delete(`/admin/chat/sessions/${id}`);
    } catch {
      // 异常忽略
    }
    this.dropLocal(id);
  }

  /**
   * 历史消息 → API messages 数组（末尾追加本轮 user 消息）。
   *
   * 三种历史 content 形态的还原：
   *  - 纯文本字符串：直接用；
   *  - 入库的多模态 JSON 串（persistContent 以 JSON.stringify 存储）：解析回内容块数组；
   *  - 内存态带 attachments 的 user 消息：重建成 text + image_url 块。
   * 失败轮（空内容/loading 占位）不上屏 —— 空回复会污染上下文。
   */
  private toApiMessages(history: ChatMessage[] | undefined, userContent: any): any[] {
    const msgs: any[] = [];
    for (const m of history ?? []) {
      if (m.status === 'loading') continue;
      if (!m.content || !m.content.trim()) continue;
      let content: any = m.content;
      if (content.startsWith('[{')) {
        try {
          const parsed = JSON.parse(content);
          if (Array.isArray(parsed)) content = parsed;
        } catch {
          // 非法 JSON 串：保持原样当纯文本
        }
      } else if (m.role === 'user' && m.attachments && m.attachments.length > 0) {
        content = [
          { type: 'text', text: m.content },
          ...m.attachments.map((a) => ({ type: 'image_url', image_url: { url: a.dataUrl } })),
        ];
      }
      msgs.push({ role: m.role, content });
    }
    msgs.push({ role: 'user', content: userContent });
    return msgs;
  }

  async sendMessageStream(options: SendMessageOptions): Promise<void> {
    const { sessionId, userMsgId, assistantMsgId, model, reasoningEffort, content, attachments, history, onChunk, onUsage, onError, onFinish } = options;

    try {
      // 组装 OpenAI 多模态消息：无附件 = 纯字符串；有附件 = text + image_url 内容块
      // （后端 facade/translate.go 原生支持 image_url → input_image 转换）
      const userContent: any = attachments && attachments.length > 0
        ? [
            { type: 'text', text: content },
            ...attachments.map((a) => ({ type: 'image_url', image_url: { url: a.dataUrl } })),
          ]
        : content;
      const persistContent = typeof userContent === 'string' ? userContent : JSON.stringify(userContent);

      // 1. 先将用户消息持久化入库（多模态内容以 JSON 串存储，读取端解析）
      await httpClient.post(`/admin/chat/sessions/${sessionId}/messages`, {
        id: userMsgId,
        role: 'user',
        content: persistContent,
        status: 'success',
      }).catch(() => {});

      // 2. 发起流式推理请求
      const response = await fetch('/v1/chat/completions', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-Oaiprism-Session': sessionId,
          // 后端启用 APIKeyAuth 时缺失此头会 401
          ...(getApiKey() ? { Authorization: 'Bearer ' + getApiKey() } : {}),
        },
        body: JSON.stringify({
          model,
          reasoning_effort: reasoningEffort,
          stream: true,
          stream_options: { include_usage: true },
          // 上游不代管对话历史：必须回传完整 messages 才有上下文。
          // 历史里含 assistant 消息时，网关会跳过自己的历史注入（避免重复）。
          messages: this.toApiMessages(history, userContent),
        }),
      });

      if (!response.ok) {
        const errText = await response.text();
        throw new Error(`上游响应失败 (${response.status}): ${errText}`);
      }

      const reader = response.body?.getReader();
      if (!reader) {
        throw new Error('当前浏览器环境不支持 ReadableStream');
      }

      const decoder = new TextDecoder('utf-8');
      let buffer = '';
      let fullAssistantText = '';
      let fullAssistantReasoning = '';

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;

        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split('\n');
        buffer = lines.pop() || '';

        for (const line of lines) {
          const trimmed = line.trim();
          if (!trimmed || !trimmed.startsWith('data:')) continue;
          const dataStr = trimmed.replace(/^data:\s*/, '').trim();
          if (dataStr === '[DONE]') {
            // 将 assistant 回复持久化入库
            await httpClient.post(`/admin/chat/sessions/${sessionId}/messages`, {
              id: assistantMsgId,
              role: 'assistant',
              content: fullAssistantText,
              reasoning: fullAssistantReasoning,
              status: 'success',
            }).catch(() => {});

            onFinish?.();
            return;
          }

          try {
            const parsed = JSON.parse(dataStr);
            // include_usage 的收尾帧：choices 为空、只带 usage
            if (parsed.usage && (!parsed.choices || parsed.choices.length === 0)) {
              onUsage?.({
                promptTokens: parsed.usage.prompt_tokens || 0,
                completionTokens: parsed.usage.completion_tokens || 0,
                totalTokens: parsed.usage.total_tokens || 0,
              });
              continue;
            }
            const delta = parsed.choices?.[0]?.delta;
            if (delta) {
              const textChunk = delta.content || '';
              const reasoningChunk = delta.reasoning_content || delta.reasoning || '';
              fullAssistantText += textChunk;
              fullAssistantReasoning += reasoningChunk;
              onChunk?.(textChunk, reasoningChunk);
            }
          } catch {
            // 忽略非 JSON 行
          }
        }
      }

      // 如果流自然结束但没有收到 [DONE]
      if (fullAssistantText || fullAssistantReasoning) {
        await httpClient.post(`/admin/chat/sessions/${sessionId}/messages`, {
          id: assistantMsgId,
          role: 'assistant',
          content: fullAssistantText,
          reasoning: fullAssistantReasoning,
          status: 'success',
        }).catch(() => {});
      }

      onFinish?.();
    } catch (err: any) {
      onError?.(err instanceof Error ? err : new Error(String(err)));
    }
  }
}
