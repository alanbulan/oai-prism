import axios from 'axios';

/**
 * API Key 的本地持久化。
 *
 * 后端启用 APIKeyAuth 后，/v1/* 与 /admin/* 都需要 Bearer；
 * 浏览器端没有安全的凭据仓库，localStorage 是该场景下的事实标准
 * （与 ApiKeyModal 的管理界面配套：生成即设为默认，可在列表里切换）。
 */
const API_KEY_STORAGE = 'oaiprism_api_key';

/**
 * 凭据事件：网关鉴权中间件拒绝（401/403 且错误码属于网关自身）时通知订阅方，
 * 由应用层弹出"连接网关"引导 —— 此前前端静默吞掉 401，页面只剩空表，
 * 看起来像账号数据丢失。
 */
const GATEWAY_AUTH_CODES = new Set(['invalid_api_key', 'admin_forbidden', 'cross_origin_forbidden']);
const authFailureListeners = new Set<(message: string) => void>();
const credentialListeners = new Set<() => void>();

export function onAuthFailure(fn: (message: string) => void): () => void {
  authFailureListeners.add(fn);
  return () => authFailureListeners.delete(fn);
}

/** 本地保存的 Key 变化（填入、切换、登录）时触发 */
export function onCredentialChange(fn: () => void): () => void {
  credentialListeners.add(fn);
  return () => credentialListeners.delete(fn);
}

export function getApiKey(): string {
  try {
    return localStorage.getItem(API_KEY_STORAGE) || '';
  } catch {
    return '';
  }
}

export function setApiKey(key: string): void {
  try {
    if (key) {
      localStorage.setItem(API_KEY_STORAGE, key);
    } else {
      localStorage.removeItem(API_KEY_STORAGE);
    }
  } catch {
    // 隐私模式下 localStorage 可能不可用：静默降级为会话内使用。
  }
  credentialListeners.forEach((fn) => fn());
}

/**
 * 基础 HTTP 客户端配置
 */
export const httpClient = axios.create({
  baseURL: '/',
  timeout: 30000,
  headers: {
    'Content-Type': 'application/json',
  },
});

httpClient.interceptors.request.use((config) => {
  // 调用方显式指定的 Authorization 优先（如保存前先校验候选 Key）
  const key = getApiKey();
  if (key && !config.headers.Authorization) {
    config.headers.Authorization = `Bearer ${key}`;
  }
  return config;
});

httpClient.interceptors.response.use(
  (response) => response,
  (error) => {
    // 两种错误体：OpenAI 风格 {error:{message}} 与管理端 {error:"..."}
    const data = error.response?.data;
    const msg =
      data?.error?.message ||
      (typeof data?.error === 'string' ? data.error : '') ||
      error.message ||
      '网络请求异常';
    const status = error.response?.status;
    if ((status === 401 || status === 403) && GATEWAY_AUTH_CODES.has(data?.error?.code)) {
      authFailureListeners.forEach((fn) => fn(msg));
    }
    return Promise.reject(new Error(msg));
  }
);
