/**
 * 对话调试领域实体与值对象
 */

export interface ChatModelInfo {
  id: string;
  name: string;
}

export type ReasoningEffort = 'low' | 'medium' | 'high' | 'xhigh';

/** 一轮推理的 token 用量（OpenAI include_usage 语义） */
export interface ChatUsage {
  promptTokens: number;
  completionTokens: number;
  totalTokens: number;
}

/** 附件（当前支持图片，走 OpenAI image_url 多模态格式，后端 translate 原生转换） */
export interface ChatAttachment {
  name: string;
  dataUrl: string; // base64 data URL
}

/** OpenAI 多模态消息内容块 */
export interface ContentPart {
  type: 'text' | 'image_url';
  text?: string;
  image_url?: { url: string };
}

export interface ChatMessage {
  id: string;
  role: 'user' | 'assistant' | 'system';
  content: string;
  attachments?: ChatAttachment[]; // 仅 user 消息：随消息持久化的图片附件
  reasoning?: string;         // 模型思考过程（ThoughtChain 呈现）
  status?: 'loading' | 'success' | 'error';
  /** user：发送时间；assistant：生成中为开始时间，完成后为完成时间（与服务端入库时间一致） */
  createdAt: string;
  /** assistant：从发送到回复完成（或失败）的耗时 */
  durationMs?: number;
  /** assistant：本轮 token 用量（网关按实际收发内容计数；输入 = 本轮发给模型的完整上下文） */
  usage?: ChatUsage;
}

/** 回复的本地附加信息（耗时、用量）：服务端只存正文，这部分留在浏览器里 */
export interface ChatMessageMeta {
  durationMs?: number;
  usage?: ChatUsage;
}

/** 会话的模型与推理强度偏好（只存浏览器本地） */
export interface ChatSessionPrefs {
  model: string;
  effort: ReasoningEffort;
}

export interface ChatSession {
  id: string;
  title: string;
  model: string;
  reasoningEffort: ReasoningEffort;
  createdAt: string;
  updatedAt: string;
  messages: ChatMessage[];
}

export interface SendMessageOptions {
  sessionId: string;
  /** 本轮 user / assistant 消息的 ID：入库用同一个 ID，刷新后本地附加信息才对得上 */
  userMsgId: string;
  assistantMsgId: string;
  content: string;
  attachments?: ChatAttachment[];
  model: string;
  reasoningEffort: ReasoningEffort;
  /** 本会话此前的对话历史（不含本轮 user 消息与 assistant 占位）。
   * 上游不代管对话历史，每次请求必须回传完整 messages 才有上下文。 */
  history?: ChatMessage[];
  onChunk?: (chunk: string, reasoningChunk?: string) => void;
  onUsage?: (usage: ChatUsage) => void;
  onError?: (err: Error) => void;
  onFinish?: () => void;
}

export interface IChatRepository {
  fetchModelCatalog(): Promise<{ mains: ChatModelInfo[]; allIds: string[] }>;
  sendMessageStream(options: SendMessageOptions): Promise<void>;
  listSessions(): Promise<ChatSession[]>;
  saveSession(session: ChatSession): Promise<void>;
  deleteSession(id: string): Promise<void>;
  // 以下只读写浏览器本地存储
  loadSessionPrefs(id: string): ChatSessionPrefs | null;
  saveSessionPrefs(id: string, prefs: ChatSessionPrefs): void;
  saveMessageMeta(sessionId: string, msgId: string, meta: ChatMessageMeta): void;
  loadActiveSessionId(): string | null;
  saveActiveSessionId(id: string | null): void;
}
