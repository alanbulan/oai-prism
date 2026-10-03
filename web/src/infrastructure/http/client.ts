import axios from 'axios';

/**
 * API Key 的本地持久化。
 *
 * 后端启用 APIKeyAuth 后，/v1/* 与 /admin/* 都需要 Bearer；
 * 浏览器端没有安全的凭据仓库，localStorage 是该场景下的事实标准
 * （与 ApiKeyModal 的管理界面配套：生成即设为默认，可在列表里切换）。
 */
const API_KEY_STORAGE = 'oaiprism_api_key';

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
  const key = getApiKey();
  if (key) {
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
    return Promise.reject(new Error(msg));
  }
);
