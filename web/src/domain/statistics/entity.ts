/**
 * 统计与监控领域实体与值对象
 * 严格对齐后端 /admin/stats 与 Prometheus /metrics 真实字段
 */

export interface GlobalAdminStats {
  uptimeSec: number;
  accountsTotal: number;
  accountsReady: number;
  projectCacheSize: number;
  captureEnabled: boolean;
  captureWritten: number;
  captureDropped: number;
  upstream: string;
  credsMode: string;
  credsFile: string;
}

export interface MetricSummary {
  totalRequests: number;
  failures: number;
  successRate: number;      // 百分比
  activeAccounts: number;
  accountsTotal: number;
  currentQPS: number;
  avgLatencyMs: number;
  /** 累计 token（网关按 o200k_base 精确计数；该功能上线前的历史请求记为 0） */
  promptTokens: number;
  completionTokens: number;
  projectCacheSize: number;
  uptimeSec: number;
}

export interface ModelUsage {
  model: string;
  requests: number;
  percentage: number;
  avgLatencyMs: number;
  promptTokens: number;
  completionTokens: number;
}

/** 近 24 小时按整点分桶的趋势点（timestamp 为桶起点，RFC3339 UTC） */
export interface TimeSeriesPoint {
  timestamp: string;
  requests: number;
  failures: number;
  tokens: number;
  qps: number;
  latency: number;
  errorRate: number;
}

export interface RequestLog {
  id: string;
  timestamp: string;
  method: string;
  path: string;
  model: string;
  accountId: string;
  statusCode: number;
  durationMs: number;
  promptTokens: number;
  completionTokens: number;
  errorMessage: string;
  clientIp: string;
  userAgent: string;
}

export interface RequestLogFilter {
  page: number;
  pageSize: number;
  model?: string;
  accountId?: string;
  /** 结果（"ok" / "failed"，失败含流式中途失败）、精确状态码（"429"）或状态码段（"4xx" / "5xx"） */
  status?: string;
}

export interface RequestLogQueryResult {
  total: number;
  page: number;
  pageSize: number;
  items: RequestLog[];
}

export interface IStatisticsRepository {
  getSummary(): Promise<MetricSummary>;
  getAdminStats(): Promise<GlobalAdminStats>;
  getModelUsages(): Promise<ModelUsage[]>;
  getTimeSeries(): Promise<TimeSeriesPoint[]>;
  getAvailableModelIds(): Promise<string[]>;
  queryRequestLogs(filter: RequestLogFilter): Promise<RequestLogQueryResult>;
}

