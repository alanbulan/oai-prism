# OAIprism Dashboard

OAIprism 网关内置的管理控制台，由网关在 `/dashboard/` 下直接托管 `web/dist`。

技术栈：React 19 · TypeScript · Vite · antd v6 · @ant-design/x · @ant-design/plots · zustand。

## 开发

```bash
pnpm install
pnpm dev        # http://localhost:5173 ，/v1 /admin /metrics 代理到 127.0.0.1:8787
pnpm build      # 类型检查 + 产出 dist/（随仓库提交，网关直接读取）
```

## 目录

```
src/
  domain/          领域实体（与后端 JSON 字段一一对应）
  application/     zustand 状态与用例
  infrastructure/  HTTP 客户端与仓储实现
  presentation/
    theme/         主题（浅色 / 深色 / 跟随系统）与品牌色板
    layouts/       应用外壳：侧栏、顶栏、运行状态
    components/    BrandLogo、StatCard、API 密钥与管理员登录弹窗
    pages/         账号与计划池、调用统计、Chat 调试台
```

## 约定

- 颜色一律取自 antd token（`theme.useToken()`）或 `index.css` 中的 `--op-*` 变量，
  不写死十六进制色值，深色模式才能整体生效；品牌光谱色板见 `theme/ThemeProvider.tsx` 的 `SPECTRUM`。
- 品牌标识与 `public/favicon.svg`、`../docs/assets/logo.svg` 同源，修改时三处保持一致。
