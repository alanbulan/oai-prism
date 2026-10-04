import type {
  GlobalAdminStats,
  MetricSummary,
  ModelUsage,
  TimeSeriesPoint,
  RequestLog,
  RequestLogFilter,
  RequestLogQueryResult,
  IStatisticsRepository,
} from '../../domain/statistics/entity';
import { pickMainModels } from '../../domain/modelFilter';
import { httpClient } from '../http/client';

export class StatisticsRepositoryImpl implements IStatisticsRepository {
  async getAdminStats(): Promise<GlobalAdminStats> {
    try {
      const res = await httpClient.get<any>('/admin/stats');
      const d = res.data || {};
      return {
        uptimeSec: d.uptime_sec || 0,
        accountsTotal: d.accounts || 0,
        accountsReady: d.accounts_ready || 0,
        projectCacheSize: d.project_cache_size || 0,
        captureEnabled: Boolean(d.capture_enabled),
        captureWritten: d.capture_written || 0,
        captureDropped: d.capture_dropped || 0,
        upstream: d.upstream || 'https://prism.openai.com',
        credsMode: d.creds_mode || 'static',
        credsFile: d.creds_file || '',
      };
    } catch {
      return {
        uptimeSec: 0,
        accountsTotal: 0,
        accountsReady: 0,
        projectCacheSize: 0,
        captureEnabled: false,
        captureWritten: 0,
        captureDropped: 0,
        upstream: 'https://prism.openai.com',
        credsMode: 'sqlite',
        credsFile: '',
      };
    }
  }

  async getSummary(): Promise<MetricSummary> {
    const adminStats = await this.getAdminStats();
    let totalReq = 0;
    let failures = 0;
    let successRate = 100.0;
    let avgLatencyMs = 0;
    let promptTokens = 0;
    let completionTokens = 0;

    try {
      const res = await httpClient.get<any>('/admin/statistics');
      const data = res.data || {};
      totalReq = data.total_requests || 0;
      failures = data.failures || 0;
      successRate = data.success_rate !== undefined ? Number(data.success_rate.toFixed(1)) : 100.0;
      avgLatencyMs = data.avg_latency_ms || 0;
      promptTokens = data.total_prompt_tokens || 0;
      completionTokens = data.total_completion_tokens || 0;
    } catch {
      // 若尚未发起请求或失败，以 0 为准
    }

    return {
      totalRequests: totalReq,
      failures,
      successRate,
      activeAccounts: adminStats.accountsReady,
      accountsTotal: adminStats.accountsTotal,
      currentQPS: totalReq > 0 ? Number((totalReq / Math.max(adminStats.uptimeSec, 1)).toFixed(2)) : 0,
      avgLatencyMs,
      promptTokens,
      completionTokens,
      projectCacheSize: adminStats.projectCacheSize,
      uptimeSec: adminStats.uptimeSec,
    };
  }

  /** 当前对外模型 id 清单（configs 的 models 映射，已剔除下线/别名）——
   *  统计聚合用它过滤 SQLite 历史流水里的旧模型。 */
  async getAvailableModelIds(): Promise<string[]> {
    try {
      const res = await httpClient.get<any>('/v1/models');
      const data = res.data?.data || [];
      return pickMainModels(data).map((m: any) => m.id);
    } catch {
      return [];
    }
  }

  async getModelUsages(): Promise<ModelUsage[]> {
    try {
      const res = await httpClient.get<any>('/admin/statistics');
      const data = res.data?.model_usages || [];
      return data.map((m: any) => ({
        model: m.model,
        requests: m.requests || 0,
        percentage: m.percentage !== undefined ? Number(m.percentage.toFixed(1)) : 0,
        avgLatencyMs: m.avg_latency_ms || 0,
        promptTokens: m.prompt_tokens || 0,
        completionTokens: m.completion_tokens || 0,
      }));
    } catch {
      return [];
    }
  }

  async getTimeSeries(): Promise<TimeSeriesPoint[]> {
    try {
      const res = await httpClient.get<any>('/admin/statistics');
      const data = res.data?.time_series || [];
      return data.map((p: any) => ({
        timestamp: p.timestamp,
        requests: p.requests || 0,
        failures: p.failures || 0,
        tokens: p.tokens || 0,
        qps: p.qps !== undefined ? Number(p.qps.toFixed(2)) : 0,
        latency: p.latency || 0,
        errorRate: p.error_rate !== undefined ? Number(p.error_rate.toFixed(1)) : 0,
      }));
    } catch {
      return [];
    }
  }

  async queryRequestLogs(filter: RequestLogFilter): Promise<RequestLogQueryResult> {
    const params = new URLSearchParams();
    params.set('page', String(filter.page || 1));
    params.set('page_size', String(filter.pageSize || 10));
    if (filter.model) params.set('model', filter.model);
    if (filter.accountId) params.set('account_id', filter.accountId);
    if (filter.status) params.set('status', filter.status);

    const res = await httpClient.get<any>(`/admin/requests?${params.toString()}`);
    const data = res.data || {};
    const items = (data.items || []).map((d: any): RequestLog => ({
      id: d.id,
      timestamp: d.timestamp,
      method: d.method || 'GET',
      path: d.path || '',
      model: d.model || '',
      accountId: d.account_id || '',
      statusCode: d.status_code || 200,
      durationMs: d.duration_ms || 0,
      promptTokens: d.prompt_tokens || 0,
      completionTokens: d.completion_tokens || 0,
      errorMessage: d.error_message || '',
      clientIp: d.client_ip || '',
      userAgent: d.user_agent || '',
    }));

    return {
      total: data.total || 0,
      page: data.page || 1,
      pageSize: data.page_size || 10,
      items,
    };
  }
}
