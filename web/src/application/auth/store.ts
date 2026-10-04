import { create } from 'zustand';
import { onAuthFailure, onCredentialChange } from '../../infrastructure/http/client';

/**
 * 网关连接状态。
 *
 * 网关存在任何 API Key 时，/admin/* 必须携带有效凭据；浏览器里没有 Key
 * （换了地址 localhost ↔ 127.0.0.1、清过缓存、管理会话随重启失效）时
 * 所有管理接口返回 401。这里把它显式化：标记"未连接"、首次自动弹出引导，
 * 凭据更新后递增 epoch，让页面重新挂载并拉取数据。
 */
interface AuthState {
  /** 网关拒绝了当前凭据 */
  blocked: boolean;
  /** 网关给出的拒绝原因 */
  reason: string;
  /** 凭据每变化一次 +1，页面以此为 key 重新挂载 */
  epoch: number;
  /** "连接网关"对话框 */
  promptOpen: boolean;
  /** 本次会话是否已自动弹出过（之后只保留横幅，不反复打扰） */
  prompted: boolean;

  openPrompt: () => void;
  closePrompt: () => void;
}

export const useAuthStore = create<AuthState>((set) => ({
  blocked: false,
  reason: '',
  epoch: 0,
  promptOpen: false,
  prompted: false,

  openPrompt: () => set({ promptOpen: true, prompted: true }),
  closePrompt: () => set({ promptOpen: false }),
}));

onAuthFailure((reason) => {
  const s = useAuthStore.getState();
  if (s.blocked && s.reason === reason) return;
  useAuthStore.setState({
    blocked: true,
    reason,
    promptOpen: s.promptOpen || !s.prompted,
    prompted: true,
  });
});

onCredentialChange(() => {
  // 乐观解除：新凭据若仍无效，下一次请求会重新标记
  useAuthStore.setState((s) => ({ blocked: false, reason: '', epoch: s.epoch + 1, promptOpen: false }));
});
