import React, { Suspense, lazy, useEffect, useState } from 'react';
import { Spin } from 'antd';
import { MainLayout } from './presentation/layouts/MainLayout';
import { NAV_KEYS, type NavKey } from './presentation/layouts/navKeys';
import { ThemeProvider } from './presentation/theme/ThemeProvider';

// 按页代码分割：antd + @ant-design/x + plots 体积大，
// 单 chunk 2.86MB 会让首屏白白等全量下载。lazy 后每页独立分包。
const AccountsPage = lazy(() =>
  import('./presentation/pages/accounts').then((m) => ({ default: m.AccountsPage })),
);
const StatisticsPage = lazy(() =>
  import('./presentation/pages/statistics').then((m) => ({ default: m.StatisticsPage })),
);
const ChatPlaygroundPage = lazy(() =>
  import('./presentation/pages/chat').then((m) => ({ default: m.ChatPlaygroundPage })),
);

const PageFallback: React.FC = () => (
  <div style={{ display: 'flex', justifyContent: 'center', padding: 80 }}>
    <Spin size="large" />
  </div>
);

/** 当前页记录在 URL hash（#/statistics），刷新与分享链接都能回到同一页 */
function readHash(): NavKey {
  const k = window.location.hash.replace(/^#\/?/, '');
  return (NAV_KEYS as readonly string[]).includes(k) ? (k as NavKey) : 'accounts';
}

export const App: React.FC = () => {
  const [currentTab, setCurrentTab] = useState<NavKey>(readHash);

  useEffect(() => {
    const onHash = () => setCurrentTab(readHash());
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);

  const navigate = (k: NavKey) => {
    if (k === currentTab) return;
    window.location.hash = `/${k}`;
  };

  return (
    <ThemeProvider>
      <MainLayout currentKey={currentTab} onKeyChange={navigate}>
        <Suspense fallback={<PageFallback />}>
          {currentTab === 'accounts' && <AccountsPage />}
          {currentTab === 'statistics' && <StatisticsPage />}
          {currentTab === 'chat' && <ChatPlaygroundPage />}
        </Suspense>
      </MainLayout>
    </ThemeProvider>
  );
};

export default App;
