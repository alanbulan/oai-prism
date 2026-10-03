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
  Modal,
  Descriptions,
  Select,
  Input,
  Tooltip,
  theme,
} from 'antd';
import {
  LineChartOutlined,
  CheckCircleOutlined,
  ReloadOutlined,
  ThunderboltOutlined,
  SearchOutlined,
  EyeOutlined,
  FieldTimeOutlined,
} from '@ant-design/icons';
import { Area, Pie } from '@ant-design/plots';
import { stripEffort } from '../../../domain/modelFilter';
import { useStatisticsStore } from '../../../application/statistics/store';
import { useAccountStore } from '../../../application/account/store';
import type { RequestLog } from '../../../domain/statistics/entity';
import { StatCard } from '../../components/StatCard';
import { SPECTRUM, MONO_FAMILY } from '../../theme/tokens';
import { useThemeMode } from '../../theme/context';

const { Text } = Typography;

/** 耗时动态格式化：<1s 用 ms，<1min 用秒，更长用分钟 */
const fmtDuration = (ms: number) => {
  if (ms < 1000) return `${ms} ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)} s`;
  return `${(ms / 60000).toFixed(1)} 分钟`;
};

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
    logsLoading,
    fetchMetrics,
    fetchRequestLogs,
    setLogsFilter,
  } = useStatisticsStore();

  const [selectedLog, setSelectedLog] = useState<RequestLog | null>(null);
  const accountNameMap = useAccountStore((st) => st.accountNameMap);
  useEffect(() => {
    if (useAccountStore.getState().accounts.length === 0) {
      useAccountStore.getState().fetchAccounts();
    }
  }, []);
  const [modelFilter, setModelFilter] = useState<string>('');
  const [statusFilter, setStatusFilter] = useState<number | undefined>(undefined);

  useEffect(() => {
    fetchMetrics();
    fetchRequestLogs(1, 10);
    const timer = setInterval(() => {
      fetchMetrics();
      fetchRequestLogs();
    }, 8000);
    return () => clearInterval(timer);
  }, [fetchMetrics, fetchRequestLogs]);

  // 格式化运行时长
  const formatUptime = (sec?: number) => {
    if (!sec) return '0秒';
    const m = Math.floor(sec / 60);
    const s = sec % 60;
    if (m > 0) return `${m}分${s}秒`;
    return `${s}秒`;
  };

  // 折线/面积图配置 (QPS 走势)：品牌色渐隐填充，深浅主题自适应
  const areaConfig = {
    data: timeSeries,
    xField: 'timestamp',
    yField: 'qps',
    shapeField: 'smooth',
    theme: chartTheme,
    axis: {
      x: { labelAutoRotate: false, labelAutoHide: true, tick: false },
      y: { grid: true, gridLineDash: [4, 4], gridStrokeOpacity: 0.6 },
    },
    style: {
      fill: `linear-gradient(-90deg, ${token.colorBgContainer} 0%, ${token.colorPrimary} 100%)`,
      fillOpacity: 0.3,
    },
    line: {
      style: {
        stroke: token.colorPrimary,
        lineWidth: 2,
      },
    },
  };

  // 现役主模型过滤：以「当前 /v1/models 清单」为准（后端 config 已剔除下线模型），
  // SQLite 历史流水里的旧模型（astra 系等）与档位变体不再出现在分布图中：
  //   剥离档位后缀 → 必须在当前清单内；变体调用量归并到主模型（加权平均时延）。
  const currentModelIds = useStatisticsStore((s) => s.currentModelIds);
  const activeUsages = useMemo(() => {
    const merged = new Map<string, { requests: number; avgLatencyMs: number }>();
    for (const u of modelUsages) {
      const main = stripEffort(u.model);
      if (currentModelIds.length > 0 && !currentModelIds.includes(main)) continue;
      const prev = merged.get(main) || { requests: 0, avgLatencyMs: 0 };
      // 加权平均时延
      const total = prev.requests + u.requests;
      const avg = total > 0 ? (prev.avgLatencyMs * prev.requests + u.avgLatencyMs * u.requests) / total : 0;
      merged.set(main, { requests: total, avgLatencyMs: Math.round(avg) });
    }
    const totalReq = [...merged.values()].reduce((s, v) => s + v.requests, 0) || 1;
    return [...merged.entries()]
      .map(([model, v]) => ({
        model,
        requests: v.requests,
        avgLatencyMs: v.avgLatencyMs,
        percentage: Math.round((v.requests / totalReq) * 1000) / 10,
      }))
      .sort((a, b) => b.requests - a.requests);
  }, [modelUsages, currentModelIds]);

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

  // 请求流水明细列
  const requestLogColumns = [
    {
      title: '时间',
      dataIndex: 'timestamp',
      key: 'timestamp',
      width: 170,
      render: (ts: string) => (
        <Text style={{ fontSize: 13, fontFamily: MONO_FAMILY, fontVariantNumeric: 'tabular-nums' }}>
          {ts ? new Date(ts).toLocaleTimeString() + ' ' + new Date(ts).toLocaleDateString() : '-'}
        </Text>
      ),
    },
    {
      title: '请求 ID',
      dataIndex: 'id',
      key: 'id',
      width: 140,
      ellipsis: { showTitle: true },
      render: (id: string) => (
        <Tooltip title={id}>
          <Text code style={{ fontSize: 12, display: 'block', maxWidth: 130, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
            {id}
          </Text>
        </Tooltip>
      ),
    },
    {
      title: '请求路径',
      dataIndex: 'path',
      key: 'path',
      render: (p: string, r: RequestLog) => (
        <Space>
          <Tag color="blue">{r.method}</Tag>
          <span style={{ fontFamily: MONO_FAMILY, fontSize: 13 }}>{p}</span>
        </Space>
      ),
    },
    {
      title: '调用模型',
      dataIndex: 'model',
      key: 'model',
      render: (m: string) =>
        m ? <Tag color={m.includes('6') ? 'volcano' : 'cyan'}>{m}</Tag> : <Text type="secondary">-</Text>,
    },
    {
      title: '处理账号',
      dataIndex: 'accountId',
      key: 'accountId',
      render: (acc: string) => {
        if (!acc) return <Text type="secondary">-</Text>;
        const name = accountNameMap.get(acc);
        return (
          <Tooltip title={name && name !== acc ? `账号：${name}` : acc}>
            <Tag color="geekblue" style={{ cursor: 'default' }}>{acc}</Tag>
          </Tooltip>
        );
      },
    },
    {
      title: '状态',
      dataIndex: 'statusCode',
      key: 'statusCode',
      width: 90,
      render: (code: number) => (
        <Tag color={code >= 200 && code < 300 ? 'success' : 'error'}>
          {code}
        </Tag>
      ),
    },
    {
      title: '耗时',
      dataIndex: 'durationMs',
      key: 'durationMs',
      width: 100,
      render: (ms: number) => (
        <span style={{
          fontWeight: 600,
          fontVariantNumeric: 'tabular-nums',
          color: ms > 60000 ? token.colorError : ms > 15000 ? token.colorWarning : token.colorSuccess
        }}>
          {fmtDuration(ms)}
        </span>
      ),
    },
    {
      title: '客户端 IP',
      dataIndex: 'clientIp',
      key: 'clientIp',
      render: (ip: string) => (
        <Text type="secondary" style={{ fontSize: 12 }}>{ip || '127.0.0.1'}</Text>
      ),
    },
    {
      title: '操作',
      key: 'action',
      width: 80,
      fixed: 'right' as const,
      render: (_: any, r: RequestLog) => (
        <Button
          type="link"
          size="small"
          icon={<EyeOutlined />}
          onClick={() => setSelectedLog(r)}
        >
          详情
        </Button>
      ),
    },
  ];

  return (
    <div className="page-fill">
      <div className="page-scroll">
      <Space orientation="vertical" size="large" style={{ width: '100%' }}>
      {/* 顶部真实指标卡片 */}
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
                失败 <Text type={summary?.failures ? 'danger' : 'secondary'} style={{ fontSize: 12 }}>{summary?.failures || 0}</Text> 次
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
            value={summary?.avgLatencyMs ? fmtDuration(summary.avgLatencyMs) : '0 ms'}
            icon={<FieldTimeOutlined />}
            color={token.colorWarning}
            loading={loading && !summary}
            footer={`已运行 ${formatUptime(summary?.uptimeSec)}`}
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="请求明细记录"
            value={requestLogsTotal.toLocaleString()}
            suffix="笔"
            icon={<ThunderboltOutlined />}
            color={SPECTRUM[5]}
            footer="SQLite 全量审计 · 每笔真实请求"
          />
        </Col>
      </Row>

      {/* 图表展示区 */}
      <Row gutter={[16, 16]}>
        <Col xs={24} lg={14}>
          <Card
            title="请求吞吐走势"
            style={{ height: '100%', display: 'flex', flexDirection: 'column', boxShadow: token.boxShadowTertiary }}
            styles={{ body: { flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column' } }}
            extra={
              <Button icon={<ReloadOutlined />} onClick={fetchMetrics} loading={loading}>
                刷新指标
              </Button>
            }
          >
            <div style={{ height: 280 }}>
              {timeSeries.length > 0 ? (
                <Area {...areaConfig} autoFit />
              ) : (
                <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无请求数据" style={{ paddingTop: 60 }} />
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
                {/* 紧凑模型列表：取代原独立「模型性能明细」表格（内容重复） */}
                <Col span={13} style={{ display: 'flex', flexDirection: 'column', justifyContent: 'center' }}>
                  {activeUsages.map((u, i) => (
                    <div
                      key={u.model}
                      style={{
                        display: 'flex',
                        alignItems: 'center',
                        gap: 8,
                        padding: '7px 0',
                        borderBottom: i < activeUsages.length - 1 ? `1px solid ${token.colorBorderSecondary}` : 'none',
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
                      <Text strong style={{ fontSize: 13, flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                        {u.model}
                      </Text>
                      <Text type="secondary" style={{ fontSize: 12, flexShrink: 0 }}>
                        {u.requests} 次
                      </Text>
                      <Text
                        type="secondary"
                        style={{ fontSize: 12, flexShrink: 0, whiteSpace: 'nowrap' }}
                        title={`平均时延 ${u.avgLatencyMs} ms`}
                      >
                        {`均 ${fmtDuration(u.avgLatencyMs)}`}
                      </Text>
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

      {/* 核心！每一笔请求明细流水表格 */}
      <Card
        style={{ boxShadow: token.boxShadowTertiary }}
        title="请求明细流水"
        extra={
          <Space wrap>
            <Input
              placeholder="按模型过滤 (如 gpt-6.1-sol)"
              allowClear
              value={modelFilter}
              onChange={(e) => setModelFilter(e.target.value)}
              onPressEnter={() => setLogsFilter({ model: modelFilter || undefined })}
              style={{ width: 180 }}
              prefix={<SearchOutlined />}
            />
            <Select
              placeholder="状态筛选"
              allowClear
              value={statusFilter}
              onChange={(val) => {
                setStatusFilter(val);
                setLogsFilter({ statusCode: val });
              }}
              style={{ width: 110 }}
              options={[
                { label: '200 OK', value: 200 },
                { label: '4xx 客户端', value: 400 },
                { label: '5xx 服务端', value: 500 },
              ]}
            />
            <Button
              icon={<ReloadOutlined />}
              onClick={() => fetchRequestLogs(logsPage, logsPageSize)}
              loading={logsLoading}
            >
              刷新流水
            </Button>
          </Space>
        }
      >
        <Table
          className="stat-log"
          rowKey="id"
          columns={requestLogColumns}
          dataSource={requestLogs}
          loading={logsLoading}
          scroll={{ x: 'max-content' }}
          pagination={{
            current: logsPage,
            pageSize: logsPageSize,
            total: requestLogsTotal,
            showSizeChanger: true,
            pageSizeOptions: ['10', '20', '50'],
            showTotal: (total) => `共 ${total} 笔`,
            onChange: (p, ps) => fetchRequestLogs(p, ps),
          }}
        />
      </Card>

      {/* 请求详情弹窗 */}
      <Modal
        title="请求流水详细信息"
        open={Boolean(selectedLog)}
        onCancel={() => setSelectedLog(null)}
        footer={[
          <Button key="close" type="primary" onClick={() => setSelectedLog(null)}>
            关闭
          </Button>,
        ]}
        width={700}
        styles={{ body: { maxHeight: '68vh', overflowY: 'auto' } }}
        destroyOnHidden
      >
        {selectedLog && (
          <Descriptions bordered column={{ xs: 1, sm: 2 }} size="small" style={{ marginTop: 12 }}>
            <Descriptions.Item label="请求 ID" span={2}>
              <Text code copyable>{selectedLog.id}</Text>
            </Descriptions.Item>
            <Descriptions.Item label="请求时间">
              <Text style={{ whiteSpace: 'nowrap' }}>
                {selectedLog.timestamp ? new Date(selectedLog.timestamp).toLocaleString('zh-CN', { hour12: false }) : '-'}
              </Text>
            </Descriptions.Item>
            <Descriptions.Item label="请求方法与路径">
              <Space size={6} style={{ maxWidth: '100%' }}>
                <Tag color="blue" style={{ flexShrink: 0 }}>{selectedLog.method}</Tag>
                <span style={{ fontFamily: MONO_FAMILY, fontSize: 12, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {selectedLog.path}
                </span>
              </Space>
            </Descriptions.Item>
            <Descriptions.Item label="命中模型">
              <Tag color="volcano">{selectedLog.model || '-'}</Tag>
            </Descriptions.Item>
            <Descriptions.Item label="路由账号">
              <Tag color="geekblue">{selectedLog.accountId || '-'}</Tag>
            </Descriptions.Item>
            <Descriptions.Item label="响应状态码">
              <Tag color={selectedLog.statusCode === 200 ? 'success' : 'error'}>
                {selectedLog.statusCode}
              </Tag>
            </Descriptions.Item>
            <Descriptions.Item label="执行耗时">
              <strong>{selectedLog.durationMs} ms</strong>
            </Descriptions.Item>
            <Descriptions.Item label="客户端 IP">
              {selectedLog.clientIp || '127.0.0.1'}
            </Descriptions.Item>
            <Descriptions.Item label="User-Agent" span={2}>
              {/* 详情弹窗的价值就是看全量信息：UA 允许多行换行展示，
                  不做单行省略 —— 截断后尾部版本号丢失反而误导排障。 */}
              <Text
                style={{
                  fontSize: 12,
                  color: token.colorTextSecondary,
                  display: 'block',
                  wordBreak: 'break-all',
                  whiteSpace: 'normal',
                  lineHeight: 1.6,
                }}
              >
                {selectedLog.userAgent || '-'}
              </Text>
            </Descriptions.Item>
            {selectedLog.errorMessage && (
              <Descriptions.Item label="异常错误摘要" span={2}>
                <Text type="danger">{selectedLog.errorMessage}</Text>
              </Descriptions.Item>
            )}
          </Descriptions>
        )}
      </Modal>
      </Space>
      </div>
    </div>
  );
};

