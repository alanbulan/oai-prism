import React, { useEffect, useState, useMemo } from 'react';
import {
  Card,
  Row,
  Col,
  Empty,
  Table,
  Space,
  Button,
  Tag,
  Typography,
  Select,
  Segmented,
  Tooltip,
  theme,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  LineChartOutlined,
  CheckCircleOutlined,
  ReloadOutlined,
  ThunderboltOutlined,
  EyeOutlined,
  FieldTimeOutlined,
} from '@ant-design/icons';
import { Column, Pie } from '@ant-design/plots';
import { stripEffort } from '../../../domain/modelFilter';
import { useStatisticsStore } from '../../../application/statistics/store';
import { useAccountStore } from '../../../application/account/store';
import type { RequestLog } from '../../../domain/statistics/entity';
import { StatCard } from '../../components/StatCard';
import { SPECTRUM, MONO_FAMILY } from '../../theme/tokens';
import { useThemeMode } from '../../theme/context';
import {
  formatCompactTime,
  formatDateTime,
  formatDuration,
  formatHour,
  formatRelative,
  formatTokens,
  formatUptime,
  normalizeIp,
  parseClient,
} from '../../utils/format';
import { isFailed, isStreamBroken, latencyTone, statusTagColor, statusText } from './logMeta';
import { RequestLogDrawer } from './RequestLogDrawer';

const { Text } = Typography;

const POLL_MS = 8000;

const STATUS_OPTIONS = [
  { label: '全部', value: '' },
  { label: '成功', value: 'ok' },
  { label: '失败', value: 'failed' },
  { label: '4xx', value: '4xx' },
  { label: '5xx', value: '5xx' },
];

export const StatisticsPage: React.FC = () => {
  const { token } = theme.useToken();
  const { isDark } = useThemeMode();
  const chartTheme = { type: isDark ? 'classicDark' : 'classic', view: { viewFill: 'transparent' } };
  const {
    summary,
    modelUsages,
    timeSeries,
    loading,
    requestLogs,
    requestLogsTotal,
    logsPage,
    logsPageSize,
    logsFilter,
    logsLoading,
    logsError,
    fetchMetrics,
    fetchRequestLogs,
    setLogsFilter,
  } = useStatisticsStore();

  // 关闭抽屉时保留 detail，收起动画期间内容不闪空
  const [detail, setDetail] = useState<RequestLog | null>(null);
  const [detailOpen, setDetailOpen] = useState(false);
  const openDetail = (r: RequestLog) => {
    setDetail(r);
    setDetailOpen(true);
  };

  const accountNameMap = useAccountStore((st) => st.accountNameMap);
  useEffect(() => {
    if (useAccountStore.getState().accounts.length === 0) {
      useAccountStore.getState().fetchAccounts();
    }
  }, []);

  useEffect(() => {
    fetchMetrics();
    fetchRequestLogs(1);
    const timer = setInterval(() => {
      fetchMetrics();
      fetchRequestLogs();
    }, POLL_MS);
    return () => clearInterval(timer);
  }, [fetchMetrics, fetchRequestLogs]);

  // 近 24 小时：成功 / 失败堆叠柱（整点分桶，本地时区标注）
  const trend = useMemo(() => {
    let total = 0;
    let failures = 0;
    let tokens = 0;
    let peak = { time: '', count: 0 };
    const data: { time: string; type: string; count: number }[] = [];
    for (const p of timeSeries) {
      const time = formatHour(p.timestamp);
      total += p.requests;
      failures += p.failures;
      tokens += p.tokens;
      if (p.requests > peak.count) peak = { time, count: p.requests };
      data.push({ time, type: '成功', count: p.requests - p.failures });
      data.push({ time, type: '失败', count: p.failures });
    }
    return { data, total, failures, tokens, peak };
  }, [timeSeries]);

  const columnConfig = {
    data: trend.data,
    xField: 'time',
    yField: 'count',
    colorField: 'type',
    stack: true,
    theme: chartTheme,
    legend: false,
    scale: { color: { domain: ['成功', '失败'], range: [token.colorPrimary, token.colorError] } },
    style: { maxWidth: 22, radius: 2 },
    axis: {
      x: { labelAutoRotate: false, labelAutoHide: true, tick: false, title: false },
      y: { grid: true, gridLineDash: [4, 4], gridStrokeOpacity: 0.6, title: false, tickCount: 4 },
    },
  };

  // 现役主模型过滤：以「当前 /v1/models 清单」为准（后端 config 已剔除下线模型），
  // SQLite 历史流水里的旧模型（astra 系等）与档位变体不再出现在分布图中：
  //   剥离档位后缀 → 必须在当前清单内；变体调用量归并到主模型（加权平均时延）。
  const currentModelIds = useStatisticsStore((s) => s.currentModelIds);
  const activeUsages = useMemo(() => {
    const merged = new Map<string, { requests: number; avgLatencyMs: number; tokens: number }>();
    for (const u of modelUsages) {
      const main = stripEffort(u.model);
      if (currentModelIds.length > 0 && !currentModelIds.includes(main)) continue;
      const prev = merged.get(main) || { requests: 0, avgLatencyMs: 0, tokens: 0 };
      // 加权平均时延
      const total = prev.requests + u.requests;
      const avg = total > 0 ? (prev.avgLatencyMs * prev.requests + u.avgLatencyMs * u.requests) / total : 0;
      merged.set(main, {
        requests: total,
        avgLatencyMs: Math.round(avg),
        tokens: prev.tokens + u.promptTokens + u.completionTokens,
      });
    }
    const totalReq = [...merged.values()].reduce((s, v) => s + v.requests, 0) || 1;
    return [...merged.entries()]
      .map(([model, v]) => ({
        model,
        requests: v.requests,
        avgLatencyMs: v.avgLatencyMs,
        tokens: v.tokens,
        percentage: Math.round((v.requests / totalReq) * 1000) / 10,
      }))
      .sort((a, b) => b.requests - a.requests);
  }, [modelUsages, currentModelIds]);

  // 流水筛选用原始模型名（含档位变体），按调用量排序
  const modelOptions = useMemo(
    () =>
      [...modelUsages]
        .sort((a, b) => b.requests - a.requests)
        .map((u) => ({ value: u.model, label: u.model })),
    [modelUsages],
  );

  // 与饼图共享的品牌光谱色板（保证图例/列表颜色一一对应）
  const MODEL_PALETTE = SPECTRUM;

  // 饼图配置 (模型调用分布)
  // 外置 label 关闭 —— 模型名/占比/请求数/均延迟全部由右侧紧凑列表承担，
  // 避免小卡片下标签被裁剪出孤立 "%"。
  const pieConfig = {
    data: activeUsages,
    angleField: 'requests',
    colorField: 'model',
    radius: 0.92,
    innerRadius: 0.66,
    theme: chartTheme,
    style: { stroke: token.colorBgContainer, lineWidth: 2 },
    scale: { color: { range: MODEL_PALETTE } },
    label: false,
    legend: false,
  };

  const toneColor = {
    fast: token.colorSuccess,
    normal: token.colorText,
    slow: token.colorWarning,
    'very-slow': token.colorError,
  } as const;

  const tabular: React.CSSProperties = { fontVariantNumeric: 'tabular-nums' };

  const requestLogColumns: ColumnsType<RequestLog> = [
    {
      title: '时间',
      dataIndex: 'timestamp',
      key: 'timestamp',
      width: 132,
      render: (ts: string) => (
        <Tooltip title={`${formatDateTime(ts)} · ${formatRelative(ts)}`}>
          <span style={{ ...tabular, color: token.colorTextSecondary }}>{formatCompactTime(ts)}</span>
        </Tooltip>
      ),
    },
    {
      title: '状态',
      dataIndex: 'statusCode',
      key: 'statusCode',
      width: 72,
      render: (code: number, r) =>
        isStreamBroken(r) ? (
          <Tooltip title={`生成中途失败（响应头已发出，HTTP ${code}）`}>
            <Tag color="error" style={{ marginInlineEnd: 0 }}>
              中断
            </Tag>
          </Tooltip>
        ) : (
          <Tooltip title={statusText(code) || undefined}>
            <Tag color={statusTagColor(code)} style={{ marginInlineEnd: 0, ...tabular }}>
              {code}
            </Tag>
          </Tooltip>
        ),
    },
    {
      // 推理入口全是 POST，方法不单独占位（详情里有）
      title: '接口',
      dataIndex: 'path',
      key: 'path',
      width: 200,
      ellipsis: true,
      render: (p: string, r) => (
        <Tooltip title={`${r.method} ${p}`}>
          <span style={{ fontFamily: MONO_FAMILY, fontSize: 13 }}>{p}</span>
        </Tooltip>
      ),
    },
    {
      title: '模型',
      dataIndex: 'model',
      key: 'model',
      width: 128,
      render: (m: string) =>
        m ? (
          <Tag bordered={false} color="processing" style={{ marginInlineEnd: 0 }}>
            {m}
          </Tag>
        ) : (
          <Text type="secondary">—</Text>
        ),
    },
    {
      title: '账号',
      dataIndex: 'accountId',
      key: 'accountId',
      width: 144,
      ellipsis: true,
      render: (acc: string) => {
        if (!acc) return <Text type="secondary">未分配</Text>;
        const name = accountNameMap.get(acc);
        return (
          <Tooltip title={name && name !== acc ? `${acc} · ${name}` : acc}>
            <span style={{ fontFamily: MONO_FAMILY, fontSize: 12 }}>{acc}</span>
          </Tooltip>
        );
      },
    },
    {
      title: 'Token',
      key: 'tokens',
      width: 104,
      align: 'right',
      render: (_, r) => {
        const total = r.promptTokens + r.completionTokens;
        if (total === 0) {
          return (
            <Tooltip title={isFailed(r) ? '失败请求不计用量' : '未记录：用量统计上线前的请求'}>
              <Text type="secondary">—</Text>
            </Tooltip>
          );
        }
        return (
          <Tooltip
            title={`输入 ${r.promptTokens.toLocaleString()} · 输出 ${r.completionTokens.toLocaleString()} · 合计 ${total.toLocaleString()}`}
          >
            <span style={{ ...tabular, fontWeight: 500 }}>{formatTokens(total)}</span>
          </Tooltip>
        );
      },
    },
    {
      title: '耗时',
      dataIndex: 'durationMs',
      key: 'durationMs',
      width: 96,
      align: 'right',
      render: (ms: number) => (
        <Tooltip title={`${ms.toLocaleString()} ms`}>
          <span style={{ ...tabular, fontWeight: 500, color: toneColor[latencyTone(ms)] }}>{formatDuration(ms)}</span>
        </Tooltip>
      ),
    },
    {
      title: '客户端',
      key: 'client',
      ellipsis: true,
      render: (_, r) => {
        const c = parseClient(r.userAgent);
        const { ip, local } = normalizeIp(r.clientIp);
        return (
          <Tooltip title={`${r.userAgent || '未携带 User-Agent'}\n${ip}${local ? '（本机）' : ''}`}>
            <span>
              {c.name}
              {c.version && <Text type="secondary"> {c.version}</Text>}
            </span>
          </Tooltip>
        );
      },
    },
    {
      title: '',
      key: 'action',
      width: 48,
      fixed: 'right',
      align: 'center',
      render: (_, r) => (
        <Tooltip title="查看详情">
          <Button
            type="text"
            size="small"
            icon={<EyeOutlined />}
            onClick={(e) => {
              e.stopPropagation();
              openDetail(r);
            }}
          />
        </Tooltip>
      ),
    },
  ];

  const legendDot = (color: string, label: string, value: number) => (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, fontSize: 12, color: token.colorTextSecondary }}>
      <span style={{ width: 8, height: 8, borderRadius: 2, background: color }} />
      {label}
      <span style={{ ...tabular, color: token.colorText, fontWeight: 500 }}>{value.toLocaleString()}</span>
    </span>
  );

  return (
    <div className="page-fill">
      <div className="page-scroll">
        <Space orientation="vertical" size="large" style={{ width: '100%' }}>
          {/* 顶部指标卡 */}
          <Row gutter={[16, 16]}>
            <Col xs={12} xl={6}>
              <StatCard
                title="累计请求"
                value={(summary?.totalRequests || 0).toLocaleString()}
                suffix="次"
                icon={<LineChartOutlined />}
                color={SPECTRUM[0]}
                loading={loading && !summary}
                footer={
                  <>
                    失败{' '}
                    <Text type={summary?.failures ? 'danger' : 'secondary'} style={{ fontSize: 12 }}>
                      {summary?.failures || 0}
                    </Text>{' '}
                    次 ·{' '}
                    <Tooltip
                      title={
                        <>
                          输入 {(summary?.promptTokens || 0).toLocaleString()} · 输出{' '}
                          {(summary?.completionTokens || 0).toLocaleString()}
                          <div style={{ marginTop: 4, opacity: 0.75 }}>
                            o200k_base 精确计数网关实际收发的内容。上游不回报用量：其内置提示词与隐藏推理不在其中，
                            用量统计上线前的请求也记为 0。
                          </div>
                        </>
                      }
                    >
                      <span>Token {formatTokens((summary?.promptTokens || 0) + (summary?.completionTokens || 0))}</span>
                    </Tooltip>
                  </>
                }
              />
            </Col>
            <Col xs={12} xl={6}>
              <StatCard
                title="请求成功率"
                value={(summary?.successRate ?? 100).toFixed(1)}
                suffix="%"
                icon={<CheckCircleOutlined />}
                color={token.colorSuccess}
                loading={loading && !summary}
                footer={`就绪账号 ${summary?.activeAccounts || 0} / ${summary?.accountsTotal || 0}`}
              />
            </Col>
            <Col xs={12} xl={6}>
              <StatCard
                title="平均处理耗时"
                value={summary?.avgLatencyMs ? formatDuration(summary.avgLatencyMs) : '—'}
                icon={<FieldTimeOutlined />}
                color={token.colorWarning}
                loading={loading && !summary}
                footer={`网关已运行 ${formatUptime(summary?.uptimeSec)}`}
              />
            </Col>
            <Col xs={12} xl={6}>
              <StatCard
                title="近 24 小时请求"
                value={trend.total.toLocaleString()}
                suffix="次"
                icon={<ThunderboltOutlined />}
                color={SPECTRUM[5]}
                loading={loading && !summary}
                footer={
                  trend.total > 0
                    ? `峰值 ${trend.peak.time} · ${trend.peak.count} 次/小时 · Token ${formatTokens(trend.tokens)}`
                    : '近 24 小时暂无请求'
                }
              />
            </Col>
          </Row>

          {/* 图表区 */}
          <Row gutter={[16, 16]}>
            <Col xs={24} lg={14}>
              <Card
                title="近 24 小时请求量"
                style={{ height: '100%', display: 'flex', flexDirection: 'column', boxShadow: token.boxShadowTertiary }}
                styles={{ body: { flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column', paddingTop: 12 } }}
                extra={
                  <Space size={16}>
                    {legendDot(token.colorPrimary, '成功', trend.total - trend.failures)}
                    {legendDot(token.colorError, '失败', trend.failures)}
                    <Tooltip title="刷新指标">
                      <Button type="text" size="small" icon={<ReloadOutlined spin={loading} />} onClick={fetchMetrics} />
                    </Tooltip>
                  </Space>
                }
              >
                <div style={{ height: 280 }}>
                  {trend.total > 0 ? (
                    <Column {...columnConfig} autoFit />
                  ) : (
                    <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="近 24 小时暂无请求" style={{ paddingTop: 60 }} />
                  )}
                </div>
              </Card>
            </Col>
            <Col xs={24} lg={10}>
              <Card
                title="模型调用分布"
                style={{ height: '100%', display: 'flex', flexDirection: 'column', boxShadow: token.boxShadowTertiary }}
                styles={{ body: { padding: '12px 16px', flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column' } }}
              >
                {activeUsages.length > 0 ? (
                  <Row gutter={8} align="middle" style={{ flex: 1, minHeight: 0 }}>
                    {/* 环形图：外部标签与图例全部关闭，信息由右侧列表承担 */}
                    <Col span={11} style={{ height: '100%', display: 'flex', alignItems: 'center' }}>
                      <div style={{ width: '100%', height: 260 }}>
                        <Pie {...pieConfig} autoFit />
                      </div>
                    </Col>
                    <Col span={13} style={{ display: 'flex', flexDirection: 'column', justifyContent: 'center' }}>
                      {activeUsages.map((u, i) => (
                        <div
                          key={u.model}
                          style={{
                            display: 'flex',
                            alignItems: 'center',
                            gap: 10,
                            padding: '8px 0',
                            borderBottom:
                              i < activeUsages.length - 1 ? `1px solid ${token.colorBorderSecondary}` : 'none',
                          }}
                        >
                          <span
                            style={{
                              width: 8,
                              height: 8,
                              borderRadius: '50%',
                              background: MODEL_PALETTE[i % MODEL_PALETTE.length],
                              flexShrink: 0,
                            }}
                          />
                          <div style={{ flex: 1, minWidth: 0 }}>
                            <Text
                              strong
                              style={{ fontSize: 13, display: 'block', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                              title={u.model}
                            >
                              {u.model}
                            </Text>
                            <Tooltip
                              title={`平均耗时 ${u.avgLatencyMs.toLocaleString()} ms · Token ${u.tokens.toLocaleString()}`}
                            >
                              <Text type="secondary" style={{ fontSize: 12, ...tabular }}>
                                {u.requests.toLocaleString()} 次 · 均 {formatDuration(u.avgLatencyMs)}
                              </Text>
                            </Tooltip>
                          </div>
                          <Text style={{ fontSize: 13, fontWeight: 600, flexShrink: 0, ...tabular }}>{u.percentage}%</Text>
                        </div>
                      ))}
                    </Col>
                  </Row>
                ) : (
                  <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无调用记录" style={{ paddingTop: 60 }} />
                )}
              </Card>
            </Col>
          </Row>

          {/* 请求明细 */}
          <Card
            style={{ boxShadow: token.boxShadowTertiary }}
            styles={{ body: { padding: '4px 0 12px' } }}
            title={
              <Space size={8}>
                <span style={{ fontWeight: 600 }}>请求明细</span>
                <Tag bordered={false} style={{ marginInlineEnd: 0, ...tabular }}>
                  {requestLogsTotal.toLocaleString()}
                </Tag>
              </Space>
            }
            extra={
              <Space size={10} wrap>
                <Select
                  placeholder="全部模型"
                  allowClear
                  showSearch
                  value={logsFilter.model}
                  onChange={(val) => setLogsFilter({ model: val || undefined })}
                  options={modelOptions}
                  style={{ width: 170 }}
                  popupMatchSelectWidth={false}
                />
                <Segmented
                  options={STATUS_OPTIONS}
                  value={logsFilter.status || ''}
                  onChange={(val) => setLogsFilter({ status: (val as string) || undefined })}
                />
                <Tooltip title={`刷新（每 ${POLL_MS / 1000} 秒自动刷新）`}>
                  <Button
                    icon={<ReloadOutlined spin={logsLoading} />}
                    onClick={() => fetchRequestLogs(logsPage, logsPageSize)}
                  />
                </Tooltip>
              </Space>
            }
          >
            <Table<RequestLog>
              className="stat-log"
              rowKey="id"
              columns={requestLogColumns}
              dataSource={requestLogs}
              loading={logsLoading && requestLogs.length === 0}
              // x = 客户端列最小 112 + 其余列宽之和；容器更宽时余量归客户端列
              scroll={{ x: 1036 }}
              onRow={(r) => ({ onClick: () => openDetail(r) })}
              locale={{
                emptyText: (
                  <Empty
                    image={Empty.PRESENTED_IMAGE_SIMPLE}
                    description={logsError ? `加载失败：${logsError}` : '暂无符合条件的请求'}
                  />
                ),
              }}
              pagination={{
                current: logsPage,
                pageSize: logsPageSize,
                total: requestLogsTotal,
                showSizeChanger: true,
                pageSizeOptions: ['10', '20', '50'],
                showTotal: (total) => `共 ${total.toLocaleString()} 笔`,
                onChange: (p, ps) => fetchRequestLogs(p, ps),
                style: { paddingInline: 16 },
              }}
            />
          </Card>

          <RequestLogDrawer log={detail} open={detailOpen} onClose={() => setDetailOpen(false)} />
        </Space>
      </div>
    </div>
  );
};
