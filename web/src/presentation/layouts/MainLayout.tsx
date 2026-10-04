import React, { useEffect, useState } from 'react';
import { Layout, Menu, Space, Button, Tooltip, message, Dropdown, Avatar, Grid, Alert, theme } from 'antd';
import type { MenuProps } from 'antd';
import {
  TeamOutlined,
  BarChartOutlined,
  CommentOutlined,
  MenuFoldOutlined,
  MenuUnfoldOutlined,
  KeyOutlined,
  UserOutlined,
  SafetyCertificateOutlined,
  LogoutOutlined,
  GithubOutlined,
  SunOutlined,
  MoonOutlined,
  DesktopOutlined,
  CheckOutlined,
  CloudServerOutlined,
  DisconnectOutlined,
} from '@ant-design/icons';
import { useAccountStore } from '../../application/account/store';
import { useAuthStore } from '../../application/auth/store';
import { ApiKeyModal } from '../components/ApiKeyModal';
import { AdminLoginModal } from '../components/AdminLoginModal';
import { ConnectGatewayModal } from '../components/ConnectGatewayModal';
import { BrandLogo } from '../components/BrandLogo';
import { useThemeMode, type ThemeMode } from '../theme/context';
import { NAV_KEYS, type NavKey } from './navKeys';

const { Header, Sider, Content } = Layout;

const REPO_URL = 'https://github.com/alanbulan/oai-prism';

const NAV: Record<NavKey, { label: string; icon: React.ReactNode; description: string }> = {
  accounts: {
    label: '账号与计划池',
    icon: <TeamOutlined />,
    description: '上游账号、凭据有效期与调度状态',
  },
  statistics: {
    label: '调用统计',
    icon: <BarChartOutlined />,
    description: '吞吐走势、成功率、模型分布与逐笔请求流水',
  },
  chat: {
    label: 'Chat 调试台',
    icon: <CommentOutlined />,
    description: '直连网关对话，验证模型、推理强度与多模态输入',
  },
};

const THEME_OPTIONS: { key: ThemeMode; label: string; icon: React.ReactNode }[] = [
  { key: 'light', label: '浅色', icon: <SunOutlined /> },
  { key: 'dark', label: '深色', icon: <MoonOutlined /> },
  { key: 'system', label: '跟随系统', icon: <DesktopOutlined /> },
];

interface MainLayoutProps {
  currentKey: NavKey;
  onKeyChange: (key: NavKey) => void;
  children: React.ReactNode;
}

export const MainLayout: React.FC<MainLayoutProps> = ({ currentKey, onKeyChange, children }) => {
  const { token } = theme.useToken();
  const screens = Grid.useBreakpoint();
  const { mode, isDark, setMode } = useThemeMode();
  const [collapsed, setCollapsed] = useState(false);
  const [apiKeyModalOpen, setApiKeyModalOpen] = useState(false);
  const [loginModalOpen, setLoginModalOpen] = useState(false);
  const { readyCount, totalCount } = useAccountStore();
  const { blocked, reason, epoch, openPrompt } = useAuthStore();

  // 顶栏的就绪状态在任何页面都要准确：页面自己没拉账号时由外壳补一次
  // （子组件 effect 先于外壳执行，loading 已置位说明页面已经在拉，无需重复）。
  // epoch 变化 = 凭据更新，需要重新拉取。
  useEffect(() => {
    const st = useAccountStore.getState();
    if (!st.loading) st.fetchAccounts();
  }, [epoch]);

  const page = NAV[currentKey];
  const healthy = !blocked && readyCount > 0;
  const statusColor = blocked ? token.colorWarning : healthy ? token.colorSuccess : token.colorError;
  const statusLabel = blocked ? '未连接网关' : healthy ? '服务就绪' : '无可用账号';

  const menuItems: MenuProps['items'] = [
    {
      type: 'group',
      label: collapsed ? null : '工作台',
      children: NAV_KEYS.map((k) => ({ key: k, icon: NAV[k].icon, label: NAV[k].label })),
    },
  ];

  const themeMenu: MenuProps = {
    selectedKeys: [mode],
    items: THEME_OPTIONS.map((o) => ({
      key: o.key,
      icon: o.icon,
      label: (
        <span style={{ display: 'inline-flex', alignItems: 'center', justifyContent: 'space-between', gap: 24, minWidth: 96 }}>
          {o.label}
          {mode === o.key && <CheckOutlined style={{ color: token.colorPrimary, fontSize: 12 }} />}
        </span>
      ),
    })),
    onClick: ({ key }) => setMode(key as ThemeMode),
  };

  const accountMenu: MenuProps = {
    items: [
      { key: 'apikey', icon: <KeyOutlined />, label: '对外 API 密钥', onClick: () => setApiKeyModalOpen(true) },
      { key: 'auth', icon: <SafetyCertificateOutlined />, label: '管理员登录', onClick: () => setLoginModalOpen(true) },
      { type: 'divider' },
      {
        key: 'logout',
        icon: <LogoutOutlined />,
        label: '退出 / 重新登录',
        danger: true,
        onClick: () => {
          message.info('请重新认证管理员凭证');
          setLoginModalOpen(true);
        },
      },
    ],
  };

  const statusDot = (
    <span
      className={healthy ? 'status-dot status-dot--live' : 'status-dot'}
      style={{ background: statusColor, color: statusColor }}
    />
  );

  return (
    <Layout style={{ height: '100vh', overflow: 'hidden' }}>
      <Sider
        collapsible
        collapsed={collapsed}
        onCollapse={setCollapsed}
        trigger={null}
        width={236}
        collapsedWidth={72}
        breakpoint="lg"
        onBreakpoint={(broken) => setCollapsed(broken)}
        style={{ borderRight: `1px solid ${token.colorBorderSecondary}`, zIndex: 10 }}
      >
        <div style={{ height: '100%', display: 'flex', flexDirection: 'column' }}>
          {/* 品牌区 */}
          <div
            style={{
              height: 60,
              display: 'flex',
              alignItems: 'center',
              gap: 10,
              padding: collapsed ? '0 20px' : '0 18px',
              flexShrink: 0,
              overflow: 'hidden',
            }}
          >
            <BrandLogo size={32} />
            {!collapsed && (
              <div style={{ lineHeight: 1.15, whiteSpace: 'nowrap' }}>
                <div style={{ fontWeight: 650, fontSize: 16, letterSpacing: -0.2, color: token.colorTextHeading }}>
                  OAIprism
                </div>
                <div style={{ fontSize: 11, color: token.colorTextTertiary, letterSpacing: 0.4 }}>Gateway Console</div>
              </div>
            )}
          </div>

          <Menu
            mode="inline"
            selectedKeys={[currentKey]}
            onClick={(e) => onKeyChange(e.key as NavKey)}
            items={menuItems}
            style={{ borderInlineEnd: 0, flex: 1, minHeight: 0, overflowY: 'auto', paddingTop: 4 }}
          />

          {/* 运行状态卡 */}
          <div style={{ padding: collapsed ? '12px 0' : 12, flexShrink: 0 }}>
            {collapsed ? (
              <Tooltip placement="right" title={blocked ? '未连接网关：点击填入 API Key' : `就绪账号 ${readyCount} / ${totalCount}`}>
                <div
                  style={{ display: 'flex', justifyContent: 'center', cursor: blocked ? 'pointer' : 'default' }}
                  onClick={blocked ? openPrompt : undefined}
                >
                  {statusDot}
                </div>
              </Tooltip>
            ) : (
              <div
                onClick={blocked ? openPrompt : undefined}
                style={{
                  border: `1px solid ${blocked ? token.colorWarningBorder : token.colorBorderSecondary}`,
                  background: blocked ? token.colorWarningBg : token.colorBgContainer,
                  borderRadius: 10,
                  padding: '10px 12px',
                  fontSize: 12,
                  cursor: blocked ? 'pointer' : 'default',
                }}
              >
                <div style={{ display: 'flex', alignItems: 'center', gap: 8, color: token.colorText }}>
                  {statusDot}
                  <span style={{ fontWeight: 500 }}>{statusLabel}</span>
                  <span style={{ marginLeft: 'auto', color: token.colorTextSecondary, fontVariantNumeric: 'tabular-nums' }}>
                    {blocked ? '—' : `${readyCount} / ${totalCount}`}
                  </span>
                </div>
                <div style={{ marginTop: 6, color: token.colorTextTertiary, display: 'flex', alignItems: 'center', gap: 6 }}>
                  {blocked ? <DisconnectOutlined /> : <CloudServerOutlined />}
                  {blocked ? '点击填入 API Key' : 'prism.openai.com'}
                </div>
              </div>
            )}
          </div>

          <div
            onClick={() => setCollapsed(!collapsed)}
            style={{
              height: 44,
              flexShrink: 0,
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
              borderTop: `1px solid ${token.colorBorderSecondary}`,
              color: token.colorTextSecondary,
              cursor: 'pointer',
            }}
          >
            {collapsed ? <MenuUnfoldOutlined /> : <MenuFoldOutlined />}
          </div>
        </div>
      </Sider>

      <Layout style={{ minWidth: 0 }}>
        <Header
          style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            gap: 16,
            borderBottom: `1px solid ${token.colorBorderSecondary}`,
            lineHeight: 'normal',
          }}
        >
          <div style={{ display: 'flex', alignItems: 'center', gap: 12, minWidth: 0 }}>
            <div
              style={{
                width: 34,
                height: 34,
                borderRadius: 9,
                display: 'grid',
                placeItems: 'center',
                fontSize: 16,
                color: token.colorPrimary,
                background: token.colorPrimaryBg,
                flexShrink: 0,
              }}
            >
              {page.icon}
            </div>
            <div style={{ minWidth: 0 }}>
              <div style={{ fontSize: 16, fontWeight: 600, color: token.colorTextHeading, whiteSpace: 'nowrap' }}>
                {page.label}
              </div>
              {screens.md && (
                <div
                  style={{
                    fontSize: 12,
                    color: token.colorTextTertiary,
                    whiteSpace: 'nowrap',
                    overflow: 'hidden',
                    textOverflow: 'ellipsis',
                  }}
                >
                  {page.description}
                </div>
              )}
            </div>
          </div>

          <Space size={4}>
            <Dropdown menu={themeMenu} trigger={['click']} placement="bottomRight">
              <Tooltip title="外观主题">
                <Button type="text" icon={isDark ? <MoonOutlined /> : <SunOutlined />} />
              </Tooltip>
            </Dropdown>
            <Tooltip title="GitHub 仓库">
              <Button type="text" icon={<GithubOutlined />} href={REPO_URL} target="_blank" rel="noreferrer" />
            </Tooltip>
            <Button icon={<KeyOutlined />} onClick={() => setApiKeyModalOpen(true)} style={{ marginInline: 8 }}>
              {screens.sm ? 'API 密钥' : null}
            </Button>
            <Dropdown menu={accountMenu} placement="bottomRight" trigger={['click']}>
              <Space size={8} style={{ cursor: 'pointer', padding: '4px 6px', borderRadius: 8 }}>
                <Avatar
                  size={28}
                  icon={<UserOutlined />}
                  style={{ background: `linear-gradient(135deg, ${token.colorPrimary}, #a855f7)` }}
                />
                {screens.md && <span style={{ fontSize: 13, fontWeight: 500, color: token.colorText }}>管理员</span>}
              </Space>
            </Dropdown>
          </Space>
        </Header>

        {/* 内容容器：body 级零滚动条，滚动收敛到各页面内部 */}
        <Content
          style={{
            flex: 1,
            minHeight: 0,
            padding: '20px 24px',
            overflow: 'hidden',
            display: 'flex',
            flexDirection: 'column',
          }}
        >
          {blocked && (
            <Alert
              type="warning"
              showIcon
              icon={<DisconnectOutlined />}
              title="未连接到网关管理接口"
              description={`${reason || 'API Key 无效或缺失'}。数据并未丢失，填入有效的 API Key 后自动恢复显示。`}
              action={
                <Button type="primary" size="small" onClick={openPrompt}>
                  填入 API Key
                </Button>
              }
              style={{ marginBottom: 16, flexShrink: 0, alignItems: 'center' }}
            />
          )}
          {/* 凭据更新后以新 key 重新挂载页面，各页 effect 自然重新拉取 */}
          <React.Fragment key={epoch}>{children}</React.Fragment>
        </Content>

        <ApiKeyModal open={apiKeyModalOpen} onClose={() => setApiKeyModalOpen(false)} />
        <AdminLoginModal open={loginModalOpen} onClose={() => setLoginModalOpen(false)} />
        <ConnectGatewayModal onAdminLogin={() => setLoginModalOpen(true)} />
      </Layout>
    </Layout>
  );
};
