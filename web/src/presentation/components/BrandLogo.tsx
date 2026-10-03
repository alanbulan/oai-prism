import React, { useId } from 'react';

/**
 * OAIprism 品牌标识：一束入射光经棱镜折射为光谱 ——
 * 一条上游协议，折射成多种标准接口。与 docs/assets/logo.svg、favicon 同源。
 */
export const BrandLogo: React.FC<{ size?: number; style?: React.CSSProperties }> = ({ size = 32, style }) => {
  const uid = useId().replace(/:/g, '');
  const bg = `bg${uid}`;
  const glow = `glow${uid}`;
  const glass = `glass${uid}`;
  const clip = `clip${uid}`;
  const spectrum = ['#ff5f6d', '#ffa53d', '#ffe04d', '#3ddc97', '#35c4ff', '#8f7dff'];

  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 128 128"
      fill="none"
      role="img"
      aria-label="OAIprism"
      style={{ display: 'block', flexShrink: 0, ...style }}
    >
      <defs>
        <linearGradient id={bg} x1="0" y1="0" x2="128" y2="128" gradientUnits="userSpaceOnUse">
          <stop offset="0" stopColor="#2b2f7a" />
          <stop offset="1" stopColor="#0c0e24" />
        </linearGradient>
        <radialGradient id={glow} cx="64" cy="62" r="54" gradientUnits="userSpaceOnUse">
          <stop offset="0" stopColor="#8b8cff" stopOpacity=".45" />
          <stop offset="1" stopColor="#8b8cff" stopOpacity="0" />
        </radialGradient>
        <linearGradient id={glass} x1="64" y1="22" x2="64" y2="94" gradientUnits="userSpaceOnUse">
          <stop offset="0" stopColor="#fff" stopOpacity=".30" />
          <stop offset="1" stopColor="#fff" stopOpacity=".06" />
        </linearGradient>
        <clipPath id={clip}>
          <rect width="128" height="128" rx="28" />
        </clipPath>
      </defs>
      <g clipPath={`url(#${clip})`}>
        <rect width="128" height="128" fill={`url(#${bg})`} />
        <rect width="128" height="128" fill={`url(#${glow})`} />
        <path d="M-6 79.5 46 58" stroke="#fff" strokeWidth="5" strokeLinecap="round" />
        <path d="M64 22 100 94H28Z" fill={`url(#${glass})`} />
        <path d="M46 58 84 62" stroke="#fff" strokeOpacity=".75" strokeWidth="3" strokeLinecap="round" />
        {spectrum.map((c, i) => (
          <path key={c} d={`M84 62 136 ${63 + i * 8} 136 ${71 + i * 8}Z`} fill={c} />
        ))}
        <path d="M64 22 100 94H28Z" stroke="#fff" strokeWidth="4.5" strokeLinejoin="round" />
      </g>
    </svg>
  );
};
