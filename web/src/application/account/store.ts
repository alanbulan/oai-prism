import { create } from 'zustand';
import type { AccountStats, AccountImportInput, AdminRefreshResponse } from '../../domain/account/entity';
import { LocalAccountRepositoryImpl } from '../../infrastructure/repositories/account.repo.impl';

const repo = new LocalAccountRepositoryImpl();

interface AccountState {
  accounts: AccountStats[];
  totalCount: number;
  readyCount: number;
  credsFile: string;
  loading: boolean;
  /** 最近一次拉取失败的原因（成功后清空） */
  error: string;
  selectedAccount: AccountStats | null;
  detailDrawerOpen: boolean;
  importModalOpen: boolean;
  editingAccount: AccountStats | null;
  editModalOpen: boolean;
  accountNameMap: Map<string, string>; // 账号 ID -> 名称（统计页 hover 用）

  // Actions
  fetchAccounts: () => Promise<void>;
  createAccount: (account: Partial<AccountStats> | Partial<AccountStats>[]) => Promise<void>;
  updateAccount: (id: string, account: Partial<AccountStats>) => Promise<void>;
  deleteAccount: (id: string) => Promise<void>;
  refreshAccount: (id: string) => Promise<AdminRefreshResponse>;
  reloadPool: () => Promise<void>;
  importAccounts: (input: AccountImportInput) => Promise<void>;
  openDetailDrawer: (account: AccountStats) => void;
  closeDetailDrawer: () => void;
  openEditModal: (account: AccountStats) => void;
  closeEditModal: () => void;
  setImportModalOpen: (open: boolean) => void;
}

export const useAccountStore = create<AccountState>((set, get) => ({
  accounts: [],
  accountNameMap: new Map(),
  totalCount: 0,
  readyCount: 0,
  credsFile: '',
  loading: false,
  error: '',
  selectedAccount: null,
  detailDrawerOpen: false,
  importModalOpen: false,
  editingAccount: null,
  editModalOpen: false,

  fetchAccounts: async () => {
    set({ loading: true });
    try {
      const data = await repo.fetchAccounts();
      const nameMap = new Map<string, string>();
      for (const a of data.accounts) nameMap.set(a.id, a.name);
      set({
        accounts: data.accounts,
        accountNameMap: nameMap,
        totalCount: data.count,
        readyCount: data.ready,
        credsFile: data.creds_file,
        error: '',
      });
    } catch (err: any) {
      // 不向上抛：页面以 error 渲染空态；鉴权失败另由 auth store 引导
      set({ error: err?.message || '加载失败' });
    } finally {
      set({ loading: false });
    }
  },

  createAccount: async (account) => {
    set({ loading: true });
    try {
      await repo.createAccount(account);
      await get().fetchAccounts();
    } finally {
      set({ loading: false });
    }
  },

  updateAccount: async (id, partial) => {
    set({ loading: true });
    try {
      await repo.updateAccount(id, partial);
      await get().fetchAccounts();
    } finally {
      set({ loading: false });
    }
  },

  deleteAccount: async (id: string) => {
    set({ loading: true });
    try {
      await repo.deleteAccount(id);
      await get().fetchAccounts();
      if (get().selectedAccount?.id === id) {
        set({ selectedAccount: null, detailDrawerOpen: false });
      }
    } finally {
      set({ loading: false });
    }
  },

  refreshAccount: async (id: string) => {
    const res = await repo.refreshAccount(id);
    await get().fetchAccounts();
    return res;
  },

  reloadPool: async () => {
    set({ loading: true });
    try {
      await repo.reloadPool();
      await get().fetchAccounts();
    } finally {
      set({ loading: false });
    }
  },

  importAccounts: async (input: AccountImportInput) => {
    const text = input.rawText.trim();
    if (!text) throw new Error('导入内容不能为空');

    // JSON 原样交给后端解析：账号对象、数组、{"accounts":[…]}、其它网关（sub2api 等）的
    // 导出文件都认，字段名不分大小写与下划线。前端自己挑字段时漏掉了嵌套的 credentials，
    // 解析不出凭据的内容也会被后端拒绝，而不是建出空账号。
    if (text.startsWith('{') || text.startsWith('[')) {
      let parsed: unknown;
      try {
        parsed = JSON.parse(text);
      } catch (e: any) {
        throw new Error(`JSON 格式有误：${e.message}`);
      }
      await get().createAccount(parsed as any);
      return;
    }

    // 其余按行：每行一串 Cookie
    const batch: any[] = [];
    const lines = text.split('\n').map((l) => l.trim()).filter(Boolean);
    for (const line of lines) {
      const id = `acc_${Date.now()}_${Math.random().toString(36).substring(2, 6)}`;
      batch.push({
        id,
        name: input.name ? `${input.name} (${id.slice(-4)})` : `Cookie账号 #${id.slice(-4)}`,
        plan: 'pro',
        cookies: line,
        max_concurrency: 2,
      });
    }

    if (batch.length === 0) {
      throw new Error('未能识别出有效的账号凭据内容');
    }

    await get().createAccount(batch);
  },

  openDetailDrawer: (account: AccountStats) => {
    set({ selectedAccount: account, detailDrawerOpen: true });
  },

  closeDetailDrawer: () => {
    set({ detailDrawerOpen: false, selectedAccount: null });
  },

  openEditModal: (account: AccountStats) => {
    set({ editingAccount: account, editModalOpen: true });
  },

  closeEditModal: () => {
    set({ editModalOpen: false, editingAccount: null });
  },

  setImportModalOpen: (open: boolean) => {
    set({ importModalOpen: open });
  },
}));
