import { create } from 'zustand';
import type { ChatAttachment, ChatMessage, ChatModelInfo, ChatSession, ChatSessionPrefs, ReasoningEffort } from '../../domain/chat/entity';
import { effortsForModel } from '../../domain/modelFilter';
import { ChatRepositoryImpl } from '../../infrastructure/repositories/chat.repo.impl';

const repo = new ChatRepositoryImpl();

const FALLBACK_MODEL = 'gpt-6.1-sol';

interface ChatState {
  models: ChatModelInfo[];
  allModelIds: string[]; // 全量 id（含档位变体）—— 推导各模型的可用推理档位
  sessions: ChatSession[];
  currentSessionId: string | null;
  /** 当前会话的模型与推理强度：每个会话各记各的（浏览器本地），切换会话时随之切换 */
  selectedModel: string;
  reasoningEffort: ReasoningEffort;
  isStreaming: boolean;
  loaded: boolean;

  // Actions
  init: () => Promise<void>;
  selectSession: (id: string) => void;
  createNewSession: () => void;
  deleteSession: (id: string) => Promise<void>;
  renameSession: (id: string, title: string) => void;
  setModel: (model: string) => void;
  setReasoningEffort: (effort: ReasoningEffort) => void;
  sendMessage: (text: string, attachments?: ChatAttachment[]) => Promise<void>;
}

/** 推理强度落到模型支持的档位上：不支持就回落 medium（各模型的档位由后端配置决定，如 6 Luna 没有 low） */
function fitEffort(model: string, allIds: string[], ...wanted: (ReasoningEffort | undefined)[]): ReasoningEffort {
  const available = effortsForModel(model, allIds);
  return (
    wanted.find((e): e is ReasoningEffort => !!e && available.includes(e)) ??
    (available.includes('medium') ? 'medium' : (available[0] ?? 'medium'))
  );
}

/**
 * 会话的模型与推理强度：本地记过的优先，其次是会话建立时的设置，最后沿用当前选择。
 * 模型已下线（不在清单里）时回落到清单第一个。
 */
function prefsFor(session: ChatSession | undefined, state: Pick<ChatState, 'models' | 'allModelIds' | 'selectedModel' | 'reasoningEffort'>): ChatSessionPrefs {
  const saved = session ? repo.loadSessionPrefs(session.id) : null;
  const ids = state.models.map((m) => m.id);
  const model =
    [saved?.model, session?.model, state.selectedModel].find((m): m is string => !!m && ids.includes(m)) ??
    ids[0] ??
    FALLBACK_MODEL;
  return {
    model,
    effort: fitEffort(model, state.allModelIds, saved?.effort, session?.reasoningEffort, state.reasoningEffort),
  };
}

/** 改写某个会话里的某条消息 */
function patchMessage(sessions: ChatSession[], sessionId: string, msgId: string, patch: Partial<ChatMessage>): ChatSession[] {
  return sessions.map((s) =>
    s.id !== sessionId ? s : { ...s, messages: s.messages.map((m) => (m.id === msgId ? { ...m, ...patch } : m)) },
  );
}

export const useChatStore = create<ChatState>((set, get) => ({
  models: [],
  allModelIds: [],
  sessions: [],
  currentSessionId: null,
  selectedModel: FALLBACK_MODEL,
  reasoningEffort: 'medium',
  isStreaming: false,
  loaded: false,

  init: async () => {
    // 生成中离开页面再回来：不重新拉取，免得把正在写入的回复覆盖掉
    if (get().isStreaming) return;
    const [{ mains, allIds }, sessions] = await Promise.all([repo.fetchModelCatalog(), repo.listSessions()]);
    // 回到上次打开的会话（页面内切换、离开页面再回来、刷新浏览器都一样）
    const keep = [get().currentSessionId, repo.loadActiveSessionId()].find((id) => id && sessions.some((s) => s.id === id));
    const currentSessionId = keep ?? sessions[0]?.id ?? null;
    const base = { models: mains, allModelIds: allIds, selectedModel: get().selectedModel, reasoningEffort: get().reasoningEffort };
    const prefs = prefsFor(sessions.find((s) => s.id === currentSessionId), base);
    set({
      models: mains,
      allModelIds: allIds,
      sessions,
      currentSessionId,
      selectedModel: prefs.model,
      reasoningEffort: prefs.effort,
      loaded: true,
    });
  },

  selectSession: (id: string) => {
    const prefs = prefsFor(get().sessions.find((s) => s.id === id), get());
    repo.saveActiveSessionId(id);
    set({ currentSessionId: id, selectedModel: prefs.model, reasoningEffort: prefs.effort });
  },

  createNewSession: () => {
    // 新会话沿用当前的模型与推理强度
    const { selectedModel, reasoningEffort } = get();
    const now = new Date().toISOString();
    const newSession: ChatSession = {
      id: `sess_${Date.now()}`,
      title: '新调试会话',
      model: selectedModel,
      reasoningEffort,
      createdAt: now,
      updatedAt: now,
      messages: [],
    };
    repo.saveSession(newSession);
    repo.saveSessionPrefs(newSession.id, { model: selectedModel, effort: reasoningEffort });
    repo.saveActiveSessionId(newSession.id);
    set({ sessions: [newSession, ...get().sessions], currentSessionId: newSession.id });
  },

  deleteSession: async (id: string) => {
    await repo.deleteSession(id);
    const sessions = get().sessions.filter((s) => s.id !== id);
    if (get().currentSessionId !== id) {
      set({ sessions });
      return;
    }
    const next = sessions[0];
    const prefs = prefsFor(next, get());
    repo.saveActiveSessionId(next?.id ?? null);
    set({ sessions, currentSessionId: next?.id ?? null, selectedModel: prefs.model, reasoningEffort: prefs.effort });
  },

  renameSession: (id: string, title: string) => {
    const trimmed = title.trim();
    if (!trimmed) return;
    const sessions = get().sessions.map((s) =>
      s.id === id ? { ...s, title: trimmed, updatedAt: new Date().toISOString() } : s,
    );
    set({ sessions });
    const updated = sessions.find((s) => s.id === id);
    if (updated) repo.saveSession(updated);
  },

  setModel: (model: string) => {
    const { allModelIds, reasoningEffort, currentSessionId } = get();
    const effort = fitEffort(model, allModelIds, reasoningEffort);
    if (currentSessionId) repo.saveSessionPrefs(currentSessionId, { model, effort });
    set({ selectedModel: model, reasoningEffort: effort });
  },

  setReasoningEffort: (effort: ReasoningEffort) => {
    const { selectedModel, currentSessionId } = get();
    if (currentSessionId) repo.saveSessionPrefs(currentSessionId, { model: selectedModel, effort });
    set({ reasoningEffort: effort });
  },

  sendMessage: async (text: string, attachments?: ChatAttachment[]) => {
    const { currentSessionId, selectedModel, reasoningEffort, sessions } = get();
    if (!text.trim() || !currentSessionId) return;

    const session = sessions.find((s) => s.id === currentSessionId);
    if (!session) return;

    const sessionId = currentSessionId;
    const started = Date.now();
    const userMsg: ChatMessage = {
      id: `msg_u_${started}`,
      role: 'user',
      content: text,
      attachments: attachments && attachments.length > 0 ? attachments : undefined,
      createdAt: new Date(started).toISOString(),
      status: 'success',
    };
    const assistantMsgId = `msg_a_${started}`;
    const assistantMsg: ChatMessage = {
      id: assistantMsgId,
      role: 'assistant',
      content: '',
      reasoning: '',
      createdAt: new Date(started).toISOString(),
      status: 'loading',
    };

    const updatedSession: ChatSession = {
      ...session,
      title: session.messages.length === 0 ? text.slice(0, 18) : session.title,
      model: selectedModel,
      reasoningEffort,
      messages: [...session.messages, userMsg, assistantMsg],
      updatedAt: new Date(started).toISOString(),
    };
    set({ sessions: sessions.map((s) => (s.id === sessionId ? updatedSession : s)), isStreaming: true });
    // 首条消息定下的标题入库；这一轮用的模型与推理强度记到会话上（刷新后照样恢复）
    if (session.messages.length === 0) repo.saveSession(updatedSession);
    repo.saveSessionPrefs(sessionId, { model: selectedModel, effort: reasoningEffort });

    let currentContent = '';
    let currentReasoning = '';

    await repo.sendMessageStream({
      sessionId,
      userMsgId: userMsg.id,
      assistantMsgId,
      content: text,
      attachments,
      model: selectedModel,
      reasoningEffort,
      // 历史轮回传给网关拼进上下文：只取到 userMsg 为止，不含 assistantMsg 占位（它此刻还是空的）。
      history: [...session.messages],
      onUsage: (usage) => set({ sessions: patchMessage(get().sessions, sessionId, assistantMsgId, { usage }) }),
      onChunk: (chunk, reasoningChunk) => {
        if (chunk) currentContent += chunk;
        if (reasoningChunk) currentReasoning += reasoningChunk;
        set({
          sessions: patchMessage(get().sessions, sessionId, assistantMsgId, {
            content: currentContent,
            reasoning: currentReasoning,
          }),
        });
      },
      onFinish: () => {
        const done = Date.now();
        const durationMs = done - started;
        const usage = get().sessions.find((s) => s.id === sessionId)?.messages.find((m) => m.id === assistantMsgId)?.usage;
        repo.saveMessageMeta(sessionId, assistantMsgId, { durationMs, usage });
        set({
          sessions: patchMessage(get().sessions, sessionId, assistantMsgId, {
            content: currentContent || '（已完成响应）',
            reasoning: currentReasoning,
            status: 'success',
            createdAt: new Date(done).toISOString(),
            durationMs,
          }),
          isStreaming: false,
        });
      },
      onError: (err) => {
        set({
          sessions: patchMessage(get().sessions, sessionId, assistantMsgId, {
            content: `[请求失败] ${err.message}`,
            status: 'error',
            durationMs: Date.now() - started,
          }),
          isStreaming: false,
        });
      },
    });
  },
}));
