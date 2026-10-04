/** 请求流水的展示规则：状态码语义与耗时分级，列表与详情共用 */

const STATUS_TEXT: Record<number, string> = {
  200: 'OK',
  201: 'Created',
  204: 'No Content',
  400: 'Bad Request',
  401: 'Unauthorized',
  403: 'Forbidden',
  404: 'Not Found',
  408: 'Request Timeout',
  413: 'Payload Too Large',
  429: 'Too Many Requests',
  499: 'Client Closed',
  500: 'Internal Error',
  502: 'Bad Gateway',
  503: 'Unavailable',
  504: 'Gateway Timeout',
};

export function statusText(code: number): string {
  return STATUS_TEXT[code] || '';
}

/** antd Tag 预设色：2xx 成功、499 客户端主动断开、4xx 警告、5xx 错误 */
export function statusTagColor(code: number): string {
  if (code >= 200 && code < 300) return 'success';
  if (code === 499) return 'default';
  if (code >= 400 && code < 500) return 'warning';
  return 'error';
}

interface LogOutcome {
  statusCode: number;
  errorMessage?: string;
}

/**
 * 流式中途失败：响应头（200）已经发出，状态码改不了，失败原因只记在 errorMessage。
 * 网关只在失败路径写 errorMessage，所以"2xx + 有错误信息"就是这一类。
 */
export function isStreamBroken(log: LogOutcome): boolean {
  return log.statusCode < 400 && !!log.errorMessage;
}

/** 失败：4xx / 5xx，或流式中途失败（与后端统计口径一致） */
export function isFailed(log: LogOutcome): boolean {
  return log.statusCode >= 400 || !!log.errorMessage;
}

export type LatencyTone = 'fast' | 'normal' | 'slow' | 'very-slow';

/** 耗时分级：流式长回复动辄几十秒，阈值按对话场景设定而非普通 API */
export function latencyTone(ms: number): LatencyTone {
  if (ms < 5_000) return 'fast';
  if (ms < 30_000) return 'normal';
  if (ms < 120_000) return 'slow';
  return 'very-slow';
}
