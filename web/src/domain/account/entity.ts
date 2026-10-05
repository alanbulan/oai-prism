/**
 * 账号与计划领域实体与值对象
 * 严格与后端 internal/account/account.go 的 Stats 结构体 1:1 对应，杜绝随意编造
 */

/** 账号状态：手动停用 / 冷却中 / 凭据失效 / 满并发 / 正常 */
export type AccountState = 'disabled' | 'cooling' | 'unusable' | 'busy' | 'ok';

export interface AccountStats {
  id: string;
  name: string;
  /** 此刻能接新请求（没停用、没冷却、凭据可用、没满并发） */
  enabled: boolean;
  /** 被手动停用：不进调度池，列表里照常显示 */
  disabled?: boolean;
  state?: AccountState;
  /** 调度优先级，数值越大越先用 */
  priority?: number;
  plan: string;
  email: string;
  has_access_token: boolean;
  has_refresh_token: boolean;
  has_session: boolean;
  token_expires?: string;
  expires_in_sec?: number;
  inflight: number;
  max_concurrency: number;
  cooldown_sec: number;
  fail_streak: number;
  total_requests: number;
  failures: number;
  last_used?: string;
  source: string;
  tags?: string[];
}

export interface AdminAccountsResponse {
  count: number;
  ready: number;
  creds_file: string;
  accounts: AccountStats[];
}

export interface AccountImportInput {
  name?: string;
  rawText: string;
}

export interface AdminRefreshResponse {
  account: string;
  source: string;
  plan: string;
  email: string;
  expires_at: string;
  user_id?: string;
  error?: string;
}
