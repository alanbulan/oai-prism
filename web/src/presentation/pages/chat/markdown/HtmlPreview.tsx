import React, { useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { Button, Modal, Tooltip, theme } from 'antd';
import {
  CheckOutlined,
  CodeOutlined,
  CompressOutlined,
  CopyOutlined,
  ExpandOutlined,
  ExportOutlined,
  EyeOutlined,
  FullscreenOutlined,
  ReloadOutlined,
} from '@ant-design/icons';
import { CodeBlock } from './CodeBlock';
import { buildPreviewDoc, buildSandboxWrapper, type PreviewKind } from './utils';

/** 首帧视口高度：按宽度取 16:10，探针上报真实尺寸后再自适应 */
const ASPECT = 0.62;
const MIN_HEIGHT = 120;
/** 极端长页面的安全上限（超出部分不显示滚动条，仍可在框内滚轮滚动） */
const MAX_HEIGHT = 4000;
/** 每次加载最多自适应几次，兜底杜绝与 vh 布局的反馈循环 */
const MAX_ADJUSTMENTS = 8;
/** 探针迟迟不回（页面脚本卡死等）时，最多等这么久就直接显示 */
const REVEAL_TIMEOUT = 1500;

interface Rect {
  x: number;
  y: number;
  w: number;
  h: number;
}

interface Measure {
  /** 文档宽度：仅当内容比消息区宽时记录（按原始宽度渲染后再缩放） */
  width: number | null;
  /** 文档高度：iframe 视口高度 */
  height: number | null;
  /** 内容本体所在矩形（去掉舞台底色、留白与投影） */
  crop: Rect | null;
  /** 舞台底色已透明化：iframe 背景也随之透明 */
  transparent: boolean;
  /** 内容四角圆角半径（左上、右上、右下、左下，文档像素）：显示区域按同样弧度裁剪 */
  radius: number[] | null;
}

const EMPTY: Measure = { width: null, height: null, crop: null, transparent: false, radius: null };

const ToolButton: React.FC<{ title: string; icon: React.ReactNode; onClick: () => void }> = ({ title, icon, onClick }) => (
  <Tooltip title={title}>
    <Button type="text" size="small" icon={icon} onClick={onClick} />
  </Tooltip>
);

interface HtmlPreviewProps {
  code: string;
  kind: PreviewKind;
  /** 源码视图用的高亮 <code> 元素 */
  source: React.ReactNode;
  lang: string;
}

/**
 * HTML / SVG 直出：没有卡片外框与标题栏，渲染结果直接出现在回复里。
 *
 * - 裁切：页内探针找出内容本体（跳过整页底色、居中留白、模糊投影这层"舞台"），
 *   只显示内容本身；工具栏可切回"整页"查看原始设计；
 * - 尺寸：无滚动条。高度随内容伸缩；内容比消息区宽时按原始宽度渲染、整体等比缩小；
 * - 安全：iframe 只给 allow-scripts，不给 allow-same-origin，生成内容运行在不透明源里，
 *   读不到控制台的 localStorage（API Key），也调不了管理接口。
 */
export const HtmlPreview: React.FC<HtmlPreviewProps> = ({ code, kind, source, lang }) => {
  const { token } = theme.useToken();
  const probeId = useId();
  const outerRef = useRef<HTMLDivElement>(null);
  const frameRef = useRef<HTMLIFrameElement>(null);
  const [mode, setMode] = useState<'preview' | 'source'>('preview');
  const [fit, setFit] = useState<'content' | 'page'>('content');
  const [runKey, setRunKey] = useState(0);
  const [fullscreen, setFullscreen] = useState(false);
  const [copied, setCopied] = useState(false);
  const [measure, setMeasure] = useState<Measure>(EMPTY);
  const [revealed, setRevealed] = useState(false);
  const adjustments = useRef(0);
  const lastGrowth = useRef(0);

  // 消息区可用宽度
  const [boxWidth, setBoxWidth] = useState(0);
  useLayoutEffect(() => {
    const el = outerRef.current;
    if (!el) return;
    setBoxWidth(el.clientWidth);
    const ro = new ResizeObserver(() => setBoxWidth(el.clientWidth));
    ro.observe(el);
    return () => ro.disconnect();
  }, [mode]);

  // 内嵌预览带探针（裁切模式下还会清掉舞台底色）；全屏与新窗口用原始页面
  const srcDoc = useMemo(
    () => buildPreviewDoc(code, kind, { id: probeId, clearStage: fit === 'content' }),
    [code, kind, probeId, fit],
  );
  const pageDoc = useMemo(() => buildPreviewDoc(code, kind), [code, kind]);

  const frameWidth = measure.width ?? boxWidth;
  const frameHeight = measure.height ?? Math.round(Math.max(320, Math.min(560, (boxWidth || 760) * ASPECT)));
  const region: Rect =
    fit === 'content' && measure.crop ? measure.crop : { x: 0, y: 0, w: frameWidth, h: frameHeight };
  const scale = boxWidth && region.w ? Math.min(1, boxWidth / region.w) : 1;
  const transparentBg = fit === 'content' && measure.transparent;
  // 裁到内容本体时，显示区域与内容同弧度：圆角外不留方角
  const corners = fit === 'content' && measure.crop && measure.radius ? measure.radius.map((r) => r * scale) : null;
  // 工具栏避开右上角的圆弧（圆角在 45° 方向内收约 0.29r）
  const toolsInset = 8 + Math.round((corners?.[1] ?? 0) * 0.29);

  // 探针迟迟不回时兜底显示
  useEffect(() => {
    if (revealed) return;
    const t = setTimeout(() => setRevealed(true), REVEAL_TIMEOUT);
    return () => clearTimeout(t);
  }, [revealed, runKey]);

  useEffect(() => {
    const onMessage = (e: MessageEvent) => {
      if (e.source !== frameRef.current?.contentWindow) return;
      const data = e.data as {
        oaiprismPreview?: string;
        width?: number;
        height?: number;
        crop?: Rect | null;
        transparent?: boolean;
        radius?: number[] | null;
      } | null;
      if (!data || data.oaiprismPreview !== probeId || typeof data.height !== 'number') return;
      const reportedW = Math.ceil(data.width || 0);
      const reportedH = Math.min(MAX_HEIGHT, Math.max(MIN_HEIGHT, Math.ceil(data.height)));

      setMeasure((prev) => {
        let { width, height } = prev;
        if (adjustments.current < MAX_ADJUSTMENTS) {
          const curW = prev.width ?? boxWidth;
          const curH = prev.height ?? frameHeight;
          // 宽度：内容溢出当前视口 → 按内容宽度渲染（随后等比缩放）
          if (boxWidth && reportedW > curW + 2) width = reportedW;
          // 高度：撑到文档高度。反馈检测 —— body{min-height:100vh}+margin 一类页面，
          // 文档永远比视口高出同一截，加高多少就跟着长多少；连续两次超出量相同即停。
          if (reportedH !== curH) {
            const growth = reportedH - curH;
            if (growth > 0 && lastGrowth.current > 0 && Math.abs(growth - lastGrowth.current) <= 2) {
              adjustments.current = MAX_ADJUSTMENTS;
            } else {
              lastGrowth.current = Math.max(growth, 0);
              height = reportedH;
            }
          }
          if (width !== prev.width || height !== prev.height) adjustments.current += 1;
        }
        const radius = Array.isArray(data.radius) && data.radius.length === 4 ? data.radius : null;
        return { width, height, crop: data.crop ?? null, transparent: Boolean(data.transparent), radius };
      });
      setRevealed(true);
    };
    window.addEventListener('message', onMessage);
    return () => window.removeEventListener('message', onMessage);
  }, [probeId, boxWidth, frameHeight]);

  // 重新运行：清空测量结果，按新一次加载重新自适应
  const rerun = () => {
    adjustments.current = 0;
    lastGrowth.current = 0;
    setMeasure(EMPTY);
    setRevealed(false);
    setRunKey((k) => k + 1);
  };

  // 裁切 / 整页切换会重载 iframe（舞台底色是否清除不同），重新开始自适应
  const toggleFit = () => {
    adjustments.current = 0;
    lastGrowth.current = 0;
    setFit((f) => (f === 'content' ? 'page' : 'content'));
  };

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // 剪贴板不可用：忽略
    }
  };

  const label = kind === 'svg' ? 'SVG' : 'HTML';

  const openInNewWindow = () => {
    const url = URL.createObjectURL(
      new Blob([buildSandboxWrapper(pageDoc, `${label} 预览 · OAIprism`)], { type: 'text/html' }),
    );
    window.open(url, '_blank', 'noopener');
    setTimeout(() => URL.revokeObjectURL(url), 60_000);
  };

  const cropped =
    measure.crop &&
    (measure.crop.x > 2 ||
      measure.crop.y > 2 ||
      measure.crop.w < frameWidth - 4 ||
      measure.crop.h < frameHeight - 4);

  if (mode === 'source') {
    return (
      <CodeBlock
        lang={lang || kind}
        code={code}
        extra={
          <Button size="small" type="text" icon={<EyeOutlined />} onClick={() => setMode('preview')}>
            渲染结果
          </Button>
        }
      >
        {source}
      </CodeBlock>
    );
  }

  return (
    <div ref={outerRef} className="op-html-direct">
      <div
        className="op-html-box"
        style={{
          width: Math.round(region.w * scale),
          height: Math.round(region.h * scale),
          opacity: revealed ? 1 : 0,
        }}
      >
        <div
          className="op-html-clip"
          style={corners ? { borderRadius: corners.map((r) => `${r.toFixed(2)}px`).join(' ') } : undefined}
        >
          {boxWidth > 0 && (
            <iframe
              key={runKey}
              ref={frameRef}
              srcDoc={srcDoc}
              title={`${label} 渲染结果`}
              sandbox="allow-scripts allow-modals"
              scrolling="no"
              style={{
                position: 'absolute',
                left: 0,
                top: 0,
                width: frameWidth,
                height: frameHeight,
                border: 'none',
                display: 'block',
                background: transparentBg ? 'transparent' : '#fff',
                colorScheme: 'light',
                transformOrigin: '0 0',
                transform: `scale(${scale}) translate(${-region.x}px, ${-region.y}px)`,
              }}
            />
          )}
        </div>
        <div
          className="op-html-tools"
          style={{ background: token.colorBgElevated, top: toolsInset, right: toolsInset }}
        >
          {cropped && (
            <ToolButton
              title={fit === 'content' ? '显示整页（含页面背景）' : '只显示内容'}
              icon={fit === 'content' ? <ExpandOutlined /> : <CompressOutlined />}
              onClick={toggleFit}
            />
          )}
          <ToolButton title="查看源码" icon={<CodeOutlined />} onClick={() => setMode('source')} />
          <ToolButton title="重新运行" icon={<ReloadOutlined />} onClick={rerun} />
          <ToolButton
            title={copied ? '已复制' : '复制源码'}
            icon={copied ? <CheckOutlined style={{ color: token.colorSuccess }} /> : <CopyOutlined />}
            onClick={copy}
          />
          <ToolButton title="新窗口打开（沙箱隔离）" icon={<ExportOutlined />} onClick={openInNewWindow} />
          <ToolButton title="全屏查看原始页面" icon={<FullscreenOutlined />} onClick={() => setFullscreen(true)} />
        </div>
      </div>

      <Modal
        open={fullscreen}
        onCancel={() => setFullscreen(false)}
        footer={null}
        width="92vw"
        centered
        destroyOnHidden
        title={`${label} 预览`}
        styles={{ body: { padding: 0 } }}
      >
        <iframe
          key={`fs-${runKey}`}
          srcDoc={pageDoc}
          title={`${label} 预览（全屏）`}
          sandbox="allow-scripts allow-modals"
          style={{ width: '100%', height: '80vh', border: 'none', display: 'block', background: '#fff', borderRadius: 8 }}
        />
      </Modal>
    </div>
  );
};
