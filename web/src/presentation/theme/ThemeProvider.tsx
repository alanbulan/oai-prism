import React, { useEffect, useMemo, useState } from 'react';
import { ConfigProvider, App as AntdApp, theme as antdTheme } from 'antd';
import type { ThemeConfig } from 'antd';
import zhCN from 'antd/locale/zh_CN';
import { ThemeContext, type ThemeMode } from './context';
import { FONT_FAMILY, MONO_FAMILY } from './tokens';

/**
 * 主题与品牌：浅色 / 深色 / 跟随系统。
 *
 * 所有页面颜色一律取自 antd token（theme.useToken），不写死十六进制色值，
 * 深浅切换只需要换 algorithm 与少量品牌覆盖值。
 * 自定义 CSS（Markdown 渲染体、滚动条等）通过 <html data-theme> 切换同一套变量。
 */

const STORAGE_KEY = 'oaiprism_theme';

function readStoredMode(): ThemeMode {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    if (v === 'light' || v === 'dark' || v === 'system') return v;
  } catch {
    // 隐私模式下 localStorage 不可用：退回跟随系统
  }
  return 'system';
}

function systemPrefersDark(): boolean {
  return typeof window !== 'undefined' && window.matchMedia?.('(prefers-color-scheme: dark)').matches;
}

function buildTheme(isDark: boolean): ThemeConfig {
  const primary = isDark ? '#7c7ffb' : '#4f46e5';
  const layoutBg = isDark ? '#0b0d12' : '#f5f6fa';
  const containerBg = isDark ? '#12151c' : '#ffffff';
  const elevatedBg = isDark ? '#181c25' : '#ffffff';
  const borderSecondary = isDark ? '#1f2430' : '#ebedf3';
  const siderBg = isDark ? '#0e1016' : '#fbfbfd';

  return {
    algorithm: isDark ? antdTheme.darkAlgorithm : antdTheme.defaultAlgorithm,
    token: {
      colorPrimary: primary,
      colorInfo: primary,
      colorLink: primary,
      colorSuccess: '#12b76a',
      colorWarning: '#f79009',
      colorError: '#f04438',
      colorBgLayout: layoutBg,
      colorBgContainer: containerBg,
      colorBgElevated: elevatedBg,
      colorBorderSecondary: borderSecondary,
      borderRadius: 8,
      borderRadiusLG: 12,
      borderRadiusSM: 6,
      fontFamily: FONT_FAMILY,
      fontFamilyCode: MONO_FAMILY,
      fontSize: 14,
      controlHeight: 34,
      wireframe: false,
      boxShadowTertiary: isDark
        ? '0 1px 2px rgba(0,0,0,0.4)'
        : '0 1px 2px rgba(16,24,40,0.04), 0 1px 3px rgba(16,24,40,0.03)',
    },
    components: {
      Layout: {
        siderBg,
        headerBg: containerBg,
        bodyBg: layoutBg,
        headerHeight: 60,
        headerPadding: '0 24px',
        triggerBg: siderBg,
      },
      Menu: {
        itemBg: 'transparent',
        subMenuItemBg: 'transparent',
        itemHeight: 40,
        itemMarginInline: 10,
        itemBorderRadius: 8,
        itemSelectedBg: isDark ? 'rgba(124,127,251,0.16)' : 'rgba(79,70,229,0.08)',
        itemSelectedColor: primary,
        itemHoverBg: isDark ? 'rgba(255,255,255,0.05)' : 'rgba(16,24,40,0.04)',
        activeBarBorderWidth: 0,
        groupTitleFontSize: 12,
      },
      Card: {
        headerFontSize: 15,
        headerHeight: 52,
      },
      Table: {
        headerBg: isDark ? '#161a22' : '#f8f9fc',
        headerColor: isDark ? 'rgba(255,255,255,0.65)' : '#475467',
        headerSplitColor: 'transparent',
        rowHoverBg: isDark ? '#171b24' : '#f9fafb',
        cellPaddingBlock: 12,
      },
      Statistic: {
        contentFontSize: 26,
      },
      Tag: {
        defaultBg: isDark ? 'rgba(255,255,255,0.06)' : '#f2f4f7',
      },
      Segmented: {
        itemSelectedBg: containerBg,
      },
    },
  };
}

export const ThemeProvider: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const [mode, setModeState] = useState<ThemeMode>(readStoredMode);
  const [sysDark, setSysDark] = useState<boolean>(systemPrefersDark);

  useEffect(() => {
    const mq = window.matchMedia?.('(prefers-color-scheme: dark)');
    if (!mq) return;
    const onChange = (e: MediaQueryListEvent) => setSysDark(e.matches);
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  }, []);

  const isDark = mode === 'dark' || (mode === 'system' && sysDark);

  useEffect(() => {
    const root = document.documentElement;
    root.dataset.theme = isDark ? 'dark' : 'light';
    root.style.colorScheme = isDark ? 'dark' : 'light';
  }, [isDark]);

  const setMode = (m: ThemeMode) => {
    setModeState(m);
    try {
      localStorage.setItem(STORAGE_KEY, m);
    } catch {
      // 忽略：仅本次会话生效
    }
  };

  const themeConfig = useMemo(() => buildTheme(isDark), [isDark]);
  const ctx = useMemo(() => ({ mode, isDark, setMode }), [mode, isDark]);

  // message / Modal.confirm 等静态方法渲染在 React 树之外，拿不到上面的 ConfigProvider；
  // 通过 holderRender 让它们同样跟随当前主题。
  useEffect(() => {
    ConfigProvider.config({
      holderRender: (children) => (
        <ConfigProvider locale={zhCN} theme={themeConfig}>
          {children}
        </ConfigProvider>
      ),
    });
  }, [themeConfig]);

  return (
    <ThemeContext.Provider value={ctx}>
      <ConfigProvider locale={zhCN} theme={themeConfig}>
        <AntdApp>{children}</AntdApp>
      </ConfigProvider>
    </ThemeContext.Provider>
  );
};
