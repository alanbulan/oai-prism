import { defineConfig } from 'vite'
import type { Plugin } from 'vite'
import react from '@vitejs/plugin-react'

/** node_modules 内模块所属的包名（兼容 pnpm 的 .pnpm/<name>@<ver>/node_modules/<name> 布局） */
function packageName(id: string): string {
  const rest = id.split(/node_modules[\\/]/).pop() || ''
  const [a, b] = rest.split(/[\\/]/)
  return a.startsWith('@') ? `${a}/${b}` : a
}

// 只有 Chat 页用到的 Markdown 渲染栈：不能落进首屏就加载的 vendor-misc
const MARKDOWN_PKG =
  /^(katex|highlight\.js|lowlight|react-markdown|unified|vfile.*|bail|trough|devlop|is-plain-obj|extend|.*(rehype|remark|micromark|mdast|hast|unist).*|property-information|space-separated-tokens|comma-separated-tokens|html-url-attributes|html-void-elements|parse5|entities|web-namespaces|zwitch|ccount|character-.*|decode-named-character-reference|markdown-table|longest-streak|trim-lines|stringify-entities|estree-util-is-identifier-name|style-to-js|style-to-object|inline-style-parser|hastscript)$/

/**
 * KaTeX 的样式为每种字体同时引用 woff2 / woff / ttf 三种格式，
 * 打包后多出几十个文件（约 1.2MB）。能跑本控制台的浏览器都支持 woff2，只保留它。
 */
function katexWoff2Only(): Plugin {
  return {
    name: 'katex-woff2-only',
    enforce: 'pre',
    transform(code, id) {
      if (!/katex(\.min)?\.css/.test(id)) return null
      return code.replace(/,\s*url\([^)]+\.(woff|ttf)\)\s*format\("(woff|truetype)"\)/g, '')
    },
  }
}

// https://vite.dev/config/
export default defineConfig({
  base: './',
  plugins: [react(), katexWoff2Only()],
  build: {
    rollupOptions: {
      output: {
        // 大依赖分包：业务代码改动不再打爆 vendor 缓存，
        // antd/x/plots 各自独立，首屏只需下载当前页需要的部分。
        manualChunks(id: string) {
          if (!id.includes('node_modules')) return undefined
          const name = packageName(id)
          // 交给打包器按引用关系放置：只有懒加载的 Chat 页引用它们，自然随该页分包
          if (MARKDOWN_PKG.test(name)) return undefined
          if (name === '@ant-design/plots' || name.startsWith('@antv')) return 'vendor-plots'
          if (name === '@ant-design/x') return 'vendor-antdx'
          if (name === 'antd' || name === '@ant-design/icons' || name.startsWith('rc-') || name.startsWith('@rc-component'))
            return 'vendor-antd'
          if (name === 'react' || name === 'react-dom' || name === 'scheduler') return 'vendor-react'
          return 'vendor-misc'
        },
      },
    },
    chunkSizeWarningLimit: 900,
  },
  server: {
    port: 5173,
    proxy: {
      '/v1': {
        target: 'http://127.0.0.1:8787',
        changeOrigin: true,
      },
      '/metrics': {
        target: 'http://127.0.0.1:8787',
        changeOrigin: true,
      },
      '/healthz': {
        target: 'http://127.0.0.1:8787',
        changeOrigin: true,
      },
      '/admin': {
        target: 'http://127.0.0.1:8787',
        changeOrigin: true,
      },
    },
  },
})
