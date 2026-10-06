import { create } from 'zustand';
import type {
  MetricSummary,
  ModelUsage,
  TimeSeriesPoint,
  RequestLog,
  RequestLogFilter,
} from '../../domain/statistics/entity';
import { StatisticsRepositoryImpl } from '../../infrastructure/repositories/statistics.repo.impl';

const repo = new StatisticsRepositoryImpl();

interface StatisticsState {
  summary: MetricSummary | null;
  modelUsages: ModelUsage[];
  timeSeries: TimeSeriesPoint[];
  loading: boolean;
  modelMainOf: Record<string, string>; // 对外模型名 → 上游模型（/v1/models），不在里面的旧模型不计入分布图

  // 请求流水明细状态
  requestLogs: RequestLog[];
  requestLogsTotal: number;
  logsPage: number;
  logsPageSize: number;
  logsFilter: { model?: string; accountId?: string; status?: string };
  logsLoading: boolean;
  logsError: string;

  fetchMetrics: () => Promise<void>;
  fetchRequestLogs: (page?: number, pageSize?: number, filter?: Partial<RequestLogFilter>) => Promise<void>;
  setLogsFilter: (filter: Partial<RequestLogFilter>) => void;
}

export const useStatisticsStore = create<StatisticsState>((set, get) => ({
  summary: null,
  modelUsages: [],
  timeSeries: [],
  loading: false,
  modelMainOf: {},

  requestLogs: [],
  requestLogsTotal: 0,
  logsPage: 1,
  logsPageSize: 10,
  logsFilter: {},
  logsLoading: false,
  logsError: '',

  fetchMetrics: async () => {
    set({ loading: true });
    try {
      const [summary, modelUsages, timeSeries, modelMainOf] = await Promise.all([
        repo.getSummary(),
        repo.getModelUsages(),
        repo.getTimeSeries(),
        repo.getModelMainMap(),
      ]);
      set({ summary, modelUsages, timeSeries, modelMainOf });
    } finally {
      set({ loading: false });
    }
  },

  fetchRequestLogs: async (page, pageSize, filter) => {
    set({ logsLoading: true });
    try {
      const curPage = page || get().logsPage;
      const curPageSize = pageSize || get().logsPageSize;
      const curFilter = { ...get().logsFilter, ...(filter || {}) };

      const res = await repo.queryRequestLogs({
        page: curPage,
        pageSize: curPageSize,
        ...curFilter,
      });

      set({
        requestLogs: res.items,
        requestLogsTotal: res.total,
        logsPage: curPage,
        logsPageSize: curPageSize,
        logsFilter: curFilter,
        logsError: '',
      });
    } catch (err: any) {
      // 8 秒轮询里抛出会变成未处理的 Promise 拒绝；记录下来交给页面空态展示
      set({ logsError: err?.message || '加载失败' });
    } finally {
      set({ logsLoading: false });
    }
  },

  setLogsFilter: (filter) => {
    set((state) => ({ logsFilter: { ...state.logsFilter, ...filter }, logsPage: 1 }));
    get().fetchRequestLogs(1, get().logsPageSize, filter);
  },
}));

