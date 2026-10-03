import React, { useEffect, useState, useMemo } from 'react';
import {
  Card,
  Row,
  Col,
  Table,
  Tag,
  Button,
  Space,
  Input,
  Select,
  Badge,
  Tooltip,
  message,
  Typography,
  Popconfirm,
  theme,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  SearchOutlined,
  ReloadOutlined,
  PlusOutlined,
  EyeOutlined,
  SyncOutlined,
  ClockCircleOutlined,
  EditOutlined,
  DeleteOutlined,
  TeamOutlined,
  CheckCircleOutlined,
  WarningOutlined,
  FieldTimeOutlined,
} from '@ant-design/icons';
import type { AccountStats } from '../../../domain/account/entity';
import { useAccountStore } from '../../../application/account/store';
import { AccountImportModal } from './AccountImportModal';
import { PlanDetailDrawer } from './PlanDetailDrawer';
import { AccountEditModal } from './AccountEditModal';
import { StatCard } from '../../components/StatCard';
import { SPECTRUM } from '../../theme/tokens';

const { Text } = Typography;

const EXPIRING_WINDOW_SEC = 7 * 86400;

export const AccountsPage: React.FC = () => {
  const { token } = theme.useToken();
  const {
    accounts,
    readyCount,
    loading,
    fetchAccounts,
    reloadPool,
    refreshAccount,
    deleteAccount,
    openDetailDrawer,
    openEditModal,
    setImportModalOpen,
  } = useAccountStore();

  // 搜索与过滤状态
  const [searchText, setSearchText] = useState('');
  const [planFilter, setPlanFilter] = useState<string>('all');
  const [statusFilter, setStatusFilter] = useState<string>('all');

  // 分页状态
  const [currentPage, setCurrentPage] = useState(1);
  const [pageSize, setPageSize] = useState(10);
  const [refreshingId, setRefreshingId] = useState<string | null>(null);

  useEffect(() => {
    fetchAccounts();
  }, [fetchAccounts]);

  // 前端多维度实时过滤
  const filteredAccounts = useMemo(() => {
    return accounts.filter((acc) => {
      const matchSearch =
        !searchText.trim() ||
        acc.name.toLowerCase().includes(searchText.toLowerCase()) ||
        acc.id.toLowerCase().includes(searchText.toLowerCase()) ||
        (acc.email && acc.email.toLowerCase().includes(searchText.toLowerCase()));

      const matchPlan =
        planFilter === 'all' || acc.plan.toLowerCase() === planFilter.toLowerCase();

      const matchStatus =
        statusFilter === 'all' ||
        (statusFilter === 'enabled' && acc.enabled) ||
        (statusFilter === 'disabled' && !acc.enabled) ||
        (statusFilter === 'cooldown' && acc.cooldown_sec > 0);

      return matchSearch && matchPlan && matchStatus;
    });
  }, [accounts, searchText, planFilter, statusFilter]);

  // 账号池概览指标
  const overview = useMemo(() => {
    let cooling = 0;
    let disabled = 0;
    let inflight = 0;
    let expiring = 0;
    let earliest: string | undefined;
    for (const a of accounts) {
      if (a.cooldown_sec > 0) cooling++;
      else if (!a.enabled) disabled++;
      inflight += a.inflight;
      if (a.token_expires && (a.expires_in_sec ?? 0) > 0) {
        if ((a.expires_in_sec ?? 0) < EXPIRING_WINDOW_SEC) expiring++;
        if (!earliest || a.token_expires < earliest) earliest = a.token_expires;
      }
    }
    return { cooling, disabled, inflight, expiring, earliest };
  }, [accounts]);

  const readyPct = accounts.length > 0 ? Math.round((readyCount / accounts.length) * 100) : 0;

  // 单账号后端刷新
  const handleSingleRefresh = async (record: AccountStats) => {
    setRefreshingId(record.id);
    try {
      const res = await refreshAccount(record.id);
      message.success(`账号 [${record.name}] 凭据已刷新！计划: ${res.plan}，到期: ${new Date(res.expires_at).toLocaleDateString()}`);
    } catch (err: any) {
      message.error(`刷新失败: ${err.message}`);
    } finally {
      setRefreshingId(null);
    }
  };

  const columns: ColumnsType<AccountStats> = [
    {
      title: '账号',
      dataIndex: 'name',
      key: 'name',
      width: 250,
      fixed: 'left',
      render: (_, record) => (
        <div style={{ minWidth: 0, lineHeight: 1.45 }}>
          <Space size={6} style={{ maxWidth: '100%' }}>
            <Tooltip title={record.name !== record.id ? record.name : undefined}>
              <Text strong style={{ cursor: 'default' }}>{record.id}</Text>
            </Tooltip>
            <Tag
              bordered={false}
              color={record.source === 'oauth' ? 'purple' : 'default'}
              style={{ fontSize: 11, lineHeight: '18px', paddingInline: 6, marginInlineEnd: 0 }}
            >
              {record.source}
            </Tag>
          </Space>
          {record.email ? (
            <Text
              type="secondary"
              copyable={{ text: record.email }}
              ellipsis
              style={{ display: 'block', fontSize: 12, maxWidth: 220 }}
            >
              {record.email}
            </Text>
          ) : (
            <Text type="secondary" style={{ display: 'block', fontSize: 12 }}>未绑定邮箱</Text>
          )}
        </div>
      ),
    },
    {
      title: '计划',
      dataIndex: 'plan',
      key: 'plan',
      width: 100,
      render: (plan) => {
        const p = (plan || 'pro').toLowerCase();
        const color = p.includes('team') || p.includes('enterprise') ? 'gold' : p.includes('pro') ? 'blue' : 'default';
        return (
          <Tag bordered={false} color={color} style={{ fontWeight: 600, textTransform: 'uppercase', marginInlineEnd: 0 }}>
            {plan || '未知'}
          </Tag>
        );
      },
    },
    {
      title: '调度状态',
      dataIndex: 'enabled',
      key: 'enabled',
      width: 120,
      render: (enabled, record) => {
        if (record.cooldown_sec > 0) {
          return (
            <Tooltip title={`冷却中：因失败过多暂时避让，剩余 ${Math.round(record.cooldown_sec)} 秒后自动解除`}>
              <Badge status="warning" text={`冷却 ${Math.round(record.cooldown_sec)}s`} />
            </Tooltip>
          );
        }
        if (enabled) {
          return <Badge status="success" text="健康可用" />;
        }
        return (
          <Tooltip title={`已停用，连续失败 ${record.fail_streak} 次`}>
            <Badge status="error" text="已停用" />
          </Tooltip>
        );
      },
    },
    {
      title: '凭据构成',
      key: 'credentials',
      width: 160,
      render: (_, record) => (
        <span className="cred-tags">
          <Tooltip title={record.has_access_token ? 'Access Token 正常' : '缺失 Access Token'}>
            <Tag bordered={false} color={record.has_access_token ? 'green' : 'default'}>JWT</Tag>
          </Tooltip>
          <Tooltip title={record.has_session ? 'Session Cookie 正常' : '缺失 Session'}>
            <Tag bordered={false} color={record.has_session ? 'blue' : 'default'}>Cookie</Tag>
          </Tooltip>
          {record.has_refresh_token && (
            <Tooltip title="支持 OAuth Refresh Token 自动续签">
              <Tag bordered={false} color="purple">OAuth</Tag>
            </Tooltip>
          )}
        </span>
      ),
    },
    {
      title: 'Token 到期',
      dataIndex: 'token_expires',
      key: 'token_expires',
      width: 120,
      render: (expires, record) => {
        if (!expires) return <Text type="secondary">永久/静态</Text>;
        const days = Math.round((record.expires_in_sec || 0) / 86400);
        return (
          <div style={{ lineHeight: 1.45 }}>
            <div style={{ fontVariantNumeric: 'tabular-nums' }}>{new Date(expires).toLocaleDateString()}</div>
            <Text type={days <= 2 ? 'danger' : 'secondary'} style={{ fontSize: 12 }}>
              <ClockCircleOutlined /> {days > 0 ? `剩余 ${days} 天` : '即将到期'}
            </Text>
          </div>
        );
      },
    },
    {
      title: <Tooltip title="在途请求 / 并发上限">并发</Tooltip>,
      key: 'concurrency',
      width: 80,
      render: (_, record) => {
        const max = record.max_concurrency > 0 ? record.max_concurrency : '∞';
        return (
          <Text style={{ fontVariantNumeric: 'tabular-nums' }}>
            <strong>{record.inflight}</strong> / {max}
          </Text>
        );
      },
    },
    {
      title: '请求 / 失败',
      key: 'stats',
      width: 100,
      render: (_, record) => (
        <Text style={{ fontVariantNumeric: 'tabular-nums' }}>
          {record.total_requests} / <Text type={record.failures > 0 ? 'danger' : 'secondary'}>{record.failures}</Text>
        </Text>
      ),
    },
    {
      title: '操作',
      key: 'actions',
      width: 150,
      fixed: 'right',
      render: (_, record) => (
        <Space size={2}>
          <Tooltip title="明细">
            <Button type="text" size="small" icon={<EyeOutlined />} onClick={() => openDetailDrawer(record)} />
          </Tooltip>
          <Tooltip title="编辑">
            <Button type="text" size="small" icon={<EditOutlined />} onClick={() => openEditModal(record)} />
          </Tooltip>
          <Tooltip title="刷新凭据">
            <Button
              type="text"
              size="small"
              icon={<SyncOutlined spin={refreshingId === record.id} />}
              disabled={refreshingId === record.id}
              onClick={() => handleSingleRefresh(record)}
            />
          </Tooltip>
          <Popconfirm
            title="确定物理删除此账号？"
            description="将直接从 SQLite 数据库与运行池中永久删除，无需后端改配置！"
            onConfirm={async () => {
              try {
                await deleteAccount(record.id);
                message.success(`账号 [${record.name}] 已从 SQLite 物理删除并生效！`);
              } catch (err: any) {
                message.error(`删除失败: ${err.message}`);
              }
            }}
            okText="确定删除"
            cancelText="取消"
            okButtonProps={{ danger: true }}
          >
            <Tooltip title="删除">
              <Button type="text" danger size="small" icon={<DeleteOutlined />} />
            </Tooltip>
          </Popconfirm>
        </Space>
      ),
    },
  ];

  return (
    <div className="page-fill" style={{ gap: 16 }}>
    <Row gutter={[16, 16]} style={{ flexShrink: 0 }}>
      <Col xs={12} xl={6}>
        <StatCard
          title="账号总数"
          value={accounts.length}
          suffix="个"
          icon={<TeamOutlined />}
          color={SPECTRUM[0]}
          loading={loading && accounts.length === 0}
          footer={`在途请求 ${overview.inflight}`}
        />
      </Col>
      <Col xs={12} xl={6}>
        <StatCard
          title="就绪可调度"
          value={readyCount}
          suffix="个"
          icon={<CheckCircleOutlined />}
          color={token.colorSuccess}
          loading={loading && accounts.length === 0}
          footer={`可调度占比 ${readyPct}%`}
        />
      </Col>
      <Col xs={12} xl={6}>
        <StatCard
          title="冷却 / 停用"
          value={overview.cooling + overview.disabled}
          suffix="个"
          icon={<WarningOutlined />}
          color={token.colorWarning}
          loading={loading && accounts.length === 0}
          footer={`冷却中 ${overview.cooling} · 已停用 ${overview.disabled}`}
        />
      </Col>
      <Col xs={12} xl={6}>
        <StatCard
          title="7 天内凭据到期"
          value={overview.expiring}
          suffix="个"
          icon={<FieldTimeOutlined />}
          color={token.colorError}
          loading={loading && accounts.length === 0}
          footer={
            overview.earliest
              ? `最早到期 ${new Date(overview.earliest).toLocaleDateString()}`
              : '无带有效期的凭据'
          }
        />
      </Col>
    </Row>
    <Card
      styles={{
        // 卡片撑满剩余高度；body 为纵向 flex：工具栏固定、表格撑满、分页贴底
        body: { padding: '16px 20px', flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column' },
      }}
      style={{ flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column', width: '100%', boxShadow: token.boxShadowTertiary }}
      title={
        <Space size={8}>
          <span style={{ fontWeight: 600 }}>账号列表</span>
          <Tag bordered={false} style={{ marginInlineEnd: 0 }}>{filteredAccounts.length}</Tag>
        </Space>
      }
      extra={
        <Space>
          <Button
            icon={<ReloadOutlined />}
            onClick={async () => {
              try {
                await reloadPool();
                message.success('账号池已从 SQLite 热重载并同步最新状态！');
              } catch (err: any) {
                message.error(`重载失败: ${err.message}`);
              }
            }}
            loading={loading}
          >
            重载并刷新
          </Button>
          <Button
            type="primary"
            icon={<PlusOutlined />}
            onClick={() => setImportModalOpen(true)}
          >
            导入新账号
          </Button>
        </Space>
      }
    >
      {/* 搜索与过滤工具栏（固定高度，不参与表格弹性） */}
      <Space orientation="horizontal" size="middle" style={{ marginBottom: 16, width: '100%', flexWrap: 'wrap', flexShrink: 0 }}>
        <Input
          placeholder="搜索账号 ID、名称或邮箱..."
          prefix={<SearchOutlined style={{ color: token.colorTextQuaternary }} />}
          value={searchText}
          onChange={(e) => {
            setSearchText(e.target.value);
            setCurrentPage(1);
          }}
          style={{ width: 260 }}
          allowClear
        />

        <Space orientation="horizontal" size="small">
          <Text type="secondary">计划等级:</Text>
          <Select
            value={planFilter}
            onChange={(v) => {
              setPlanFilter(v);
              setCurrentPage(1);
            }}
            style={{ width: 120 }}
            options={[
              { value: 'all', label: '全部计划' },
              { value: 'pro', label: 'Pro 计划' },
              { value: 'team', label: 'Team 计划' },
              { value: 'free', label: 'Free 计划' },
            ]}
          />
        </Space>

        <Space orientation="horizontal" size="small">
          <Text type="secondary">调度状态:</Text>
          <Select
            value={statusFilter}
            onChange={(v) => {
              setStatusFilter(v);
              setCurrentPage(1);
            }}
            style={{ width: 130 }}
            options={[
              { value: 'all', label: '全部状态' },
              { value: 'enabled', label: '健康可用' },
              { value: 'cooldown', label: '冷却中' },
              { value: 'disabled', label: '已停用' },
            ]}
          />
        </Space>

        {(searchText || planFilter !== 'all' || statusFilter !== 'all') && (
          <Button
            type="link"
            size="small"
            onClick={() => {
              setSearchText('');
              setPlanFilter('all');
              setStatusFilter('all');
              setCurrentPage(1);
            }}
          >
            重置筛选
          </Button>
        )}
      </Space>

      {/* 标准自适应分页表格：表格撑满剩余高度、行多时内部滚动、分页固定底部（.table-fill） */}
      <div className="table-fill">
        <Table
          rowKey="id"
          columns={columns}
          dataSource={filteredAccounts}
          loading={loading}
          scroll={{ x: 1080, y: 200 }}
          pagination={{
            current: currentPage,
            pageSize: pageSize,
            total: filteredAccounts.length,
            showSizeChanger: true,
            pageSizeOptions: ['5', '10', '20', '50'],
            showQuickJumper: true,
            showTotal: (total, range) => `第 ${range[0]}-${range[1]} 条 / 共 ${total} 条账号`,
            onChange: (page, size) => {
              setCurrentPage(page);
              setPageSize(size);
            },
          }}
        />
      </div>

      <AccountImportModal />
      <PlanDetailDrawer />
      <AccountEditModal />
    </Card>
    </div>
  );
};
