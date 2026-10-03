<div align="center">

<img src="docs/assets/logo.svg" width="108" alt="OAIprism">

<h1>OAIprism</h1>

<p><b>把 OpenAI Prism 折射成你熟悉的标准 API</b></p>

<p>
  Go 单二进制网关：OpenAI Chat / Responses、Anthropic Messages、Codex CLI 开箱即用。<br>
  多账号池调度，凭据自动续期，内置管理控制台。
</p>

<p>
  <a href="https://github.com/alanbulan/oai-prism/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/alanbulan/oai-prism/ci.yml?branch=master&style=flat-square&label=CI&logo=githubactions&logoColor=white" alt="CI"></a>
  <a href="go.mod"><img src="https://img.shields.io/github/go-mod/go-version/alanbulan/oai-prism?style=flat-square&logo=go&logoColor=white&color=00ADD8" alt="Go"></a>
  <a href="web/package.json"><img src="https://img.shields.io/badge/React-19-61DAFB?style=flat-square&logo=react&logoColor=white" alt="React 19"></a>
  <img src="https://img.shields.io/badge/CGO-free-4f46e5?style=flat-square" alt="CGO free">
  <a href="https://github.com/alanbulan/oai-prism/stargazers"><img src="https://img.shields.io/github/stars/alanbulan/oai-prism?style=flat-square&logo=github&color=f5c518" alt="Stars"></a>
  <a href="https://github.com/alanbulan/oai-prism/network/members"><img src="https://img.shields.io/github/forks/alanbulan/oai-prism?style=flat-square&logo=github" alt="Forks"></a>
  <a href="https://github.com/alanbulan/oai-prism/commits/master"><img src="https://img.shields.io/github/last-commit/alanbulan/oai-prism?style=flat-square" alt="Last commit"></a>
</p>

<p>
  <img src="https://img.shields.io/badge/OpenAI-Chat%20%C2%B7%20Responses-10a37f?style=flat-square" alt="OpenAI compatible">
  <img src="https://img.shields.io/badge/Anthropic-Messages-d97757?style=flat-square" alt="Anthropic compatible">
  <img src="https://img.shields.io/badge/Codex%20CLI-ready-111827?style=flat-square" alt="Codex CLI ready">
</p>

<p>
  <a href="#快速开始">快速开始</a> ·
  <a href="docs/使用指南.md">使用指南</a> ·
  <a href="docs/架构与原理.md">架构与原理</a> ·
  <a href="#控制台">控制台</a> ·
  <a href="https://github.com/alanbulan/oai-prism/issues">反馈问题</a>
</p>

<br>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://github.com/alanbulan/oai-prism/raw/master/docs/assets/screenshot-statistics-dark.png">
  <img src="docs/assets/screenshot-statistics-light.png" alt="OAIprism 控制台" width="100%">
</picture>

</div>

<br>

## 为什么是 OAIprism

Prism（`prism.openai.com`）背后是强大的推理模型，但它说的是一套私有协议：
start + 轮询、`turn_state` 续令牌、浏览器指纹、Sentinel 反自动化令牌、容器沙箱……
OAIprism 把这些全部收进网关，对外只暴露你已经在用的标准接口 ——
**改一个 `base_url`，现有的 SDK、CLI 和工具链就能直接用上。**

<table>
  <tr>
    <td width="33%" valign="top">
      <b>🔌 标准协议，零改造接入</b><br><br>
      OpenAI Chat Completions / Responses、Anthropic Messages 全兼容，含完整的流式事件序列。
    </td>
    <td width="33%" valign="top">
      <b>🛠️ Codex 工具桥</b><br><br>
      上游负责思考，本地 Codex CLI 负责执行。文件真实落在你的磁盘上，修改按"定位原文 → 替换"安全落地。
    </td>
    <td width="33%" valign="top">
      <b>🧠 可靠的多轮记忆</b><br><br>
      每轮发送完整上下文，经真实请求对照实验校准；会话按 API Key 指纹隔离，互不可见。
    </td>
  </tr>
  <tr>
    <td width="33%" valign="top">
      <b>⚖️ 多账号池</b><br><br>
      并发闸门、会话粘性、失败冷却、凭据自动续期、SQLite 持久化，运行中热重载。
    </td>
    <td width="33%" valign="top">
      <b>📊 内置控制台</b><br><br>
      账号管理、API Key 签发、吞吐与模型分布、逐笔请求审计、对话调试台，支持深色模式。
    </td>
    <td width="33%" valign="top">
      <b>🛡️ 默认安全</b><br><br>
      无 Key 时只放行本机；管理端与 API 权限分离；SSRF 防护、跨站写保护，管理接口从不回传凭据。
    </td>
  </tr>
</table>

## 快速开始

> 需要 Go 1.26+，以及运行 Sentinel oracle 所需的 Node.js 与 Chrome。以下命令以 Windows PowerShell 为例，
> 其它平台的手动启动方式见[使用指南](docs/使用指南.md#安装与启动)。

```powershell
git clone https://github.com/alanbulan/oai-prism.git
cd oai-prism

# 1. 编译
go build -o oaiprism.exe ./cmd/oaiprism

# 2. 导入账号：粘贴 prism.openai.com 的 Cookie、access token 或 refresh token
.\oaiprism.exe import -stdin -id main

# 3. 一键启动：Sentinel oracle → TLS 桥 → 网关
.\tools\start_bridge.ps1
```

启动后打开 **http://127.0.0.1:8787/dashboard/**，在右上角「API 密钥」生成一个 Key，然后调用：

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer $OAIPRISM_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-6.1-sol","stream":true,"messages":[{"role":"user","content":"你好"}]}'
```

## 接入客户端

**Codex CLI**：写入 `~/.codex/config.toml`，即可让 Codex 在本地执行、由 Prism 推理。

```toml
model_provider = "oaiprism"
model = "gpt-6.1-sol"
model_context_window = 16384

[model_providers.oaiprism]
name = "oaiprism"
wire_api = "responses"
requires_openai_auth = false
base_url = "http://127.0.0.1:8787/v1"
experimental_bearer_token = "<你的 Key>"
```

<details>
<summary><b>OpenAI SDK</b></summary>

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8787/v1", api_key="<你的 Key>")
resp = client.chat.completions.create(
    model="gpt-6.1-sol",
    messages=[{"role": "user", "content": "你好"}],
)
print(resp.choices[0].message.content)
```

</details>

<details>
<summary><b>Anthropic SDK</b></summary>

```python
import anthropic

client = anthropic.Anthropic(base_url="http://127.0.0.1:8787", api_key="<你的 Key>")
msg = client.messages.create(
    model="gpt-6.1-sol",
    max_tokens=1024,
    messages=[{"role": "user", "content": "你好"}],
)
print(msg.content[0].text)
```

</details>

<details>
<summary><b>可用模型</b></summary>

| 模型 | 推理强度变体 |
|---|---|
| `gpt-6.1-sol`（默认） | `-low` / `-high` / `-xhigh` |
| `gpt-6-luna` | `-high` / `-xhigh` |
| `gpt-5.6-sol` | `-low` / `-high` / `-xhigh` |
| `gpt-5.6-terra` | `-high` / `-xhigh` |

以 `GET /v1/models` 为准，映射表在 `facade.models` 中可按需增删。

</details>

## 控制台

内置于网关的 `/dashboard/`，无需额外部署。浅色、深色、跟随系统三种外观。

<p align="center">
  <img src="docs/assets/screenshot-accounts.png" alt="账号与计划池" width="48%">
  &nbsp;
  <img src="docs/assets/screenshot-chat.png" alt="Chat 调试台" width="48%">
</p>

<p align="center">
  <sub><b>账号与计划池</b>：健康状态、凭据有效期、并发与失败统计 &nbsp;·&nbsp; <b>Chat 调试台</b>：切换模型与推理强度，HTML / SVG 即时预览</sub>
</p>

## 架构

```mermaid
flowchart LR
    C["OpenAI SDK<br/>Anthropic SDK<br/>Codex CLI"]

    subgraph GW["OAIprism 网关 · :8787"]
        direction TB
        F["协议门面<br/>Chat · Responses · Messages"]
        T["Codex 工具桥"]
        P["账号池<br/>调度 · 粘性 · 续期"]
        D["控制台 · 管理 API"]
    end

    B["TLS 桥 · :8790<br/>浏览器指纹传输"]
    O["Sentinel Oracle · :8791<br/>签发反自动化令牌"]
    U[("prism.openai.com")]

    C -->|标准 API| F
    F --- T
    F --> P
    D -.->|管理| P
    P -->|start + 轮询| B
    B -->|取令牌| O
    B --> U
```

客户端请求在网关被翻译为上游的 start + 轮询协议，经 TLS 桥以浏览器指纹发出；
轮询结果再被还原为 token 级增量 SSE 推回客户端。三个进程各自独立、崩溃面小、可单独重启。

深入阅读：[多轮上下文](docs/架构与原理.md#多轮上下文) ·
[Codex 工具桥](docs/架构与原理.md#codex-工具桥) ·
[上游协议](docs/架构与原理.md#上游协议已实测校准) ·
[沙箱](docs/架构与原理.md#沙箱) ·
[安全设计](docs/架构与原理.md#安全设计) ·
[性能设计](docs/架构与原理.md#性能设计)

## 文档

| 文档 | 内容 |
|---|---|
| [使用指南](docs/使用指南.md) | 安装启动、鉴权与 API Key、客户端接入、端点与控制头、运维配置、部署回滚 |
| [架构与原理](docs/架构与原理.md) | 系统架构、多轮上下文、工具桥、上游协议、沙箱、安全与性能设计、目录结构 |
| [协议校准报告](docs/协议校准报告.md) | 上游推理协议的实测字段与行为 |
| [Prism 完整调用链](docs/Prism完整调用链.md) | 从建项目到生成完成的全部请求 |
| [Yjs 依赖调研与沙箱方案](docs/Yjs依赖调研与沙箱方案.md) | 沙箱同步机制与前置流程 |
| [部署报告](docs/PrismOpenAIProxy-部署报告.md) · [对齐核对](docs/PrismOpenAIProxy-对齐核对.md) | 部署实录与接口对齐清单 |

## 开发

```bash
# 提交前本地门禁（与 CI 完全一致）
go vet ./... && go test ./... && go test -race ./...
cd web && pnpm install && npx tsc --noEmit && pnpm build
```

- `-race` 不可省略：账号池、粘性表、SSE 写出、会话链、热重载都是并发结构；
- 测试包含**模拟上游**的端到端用例，以及在 pwsh、Windows PowerShell、bash + python3 中
  **真实执行**合成补丁脚本、逐字节核对落盘结果的回归测试；
- CI 在任意分支 push / PR 时运行 Go 门禁（vet → test → race → 构建 → 交叉编译 linux/amd64、arm64）
  与 Dashboard 门禁（tsc → vite build）。

欢迎提交 Issue 与 Pull Request。较大的改动请先开 Issue 讨论方向。

## Star History

<a href="https://star-history.com/#alanbulan/oai-prism&Date">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=alanbulan/oai-prism&type=Date&theme=dark">
    <img src="https://api.star-history.com/svg?repos=alanbulan/oai-prism&type=Date" alt="Star History Chart" width="100%">
  </picture>
</a>

## 声明

OAIprism 是独立的社区项目，与 OpenAI 不存在任何隶属、合作或背书关系。
使用前请自行确认符合上游服务条款；账号凭据只保存在本机，仅用于与 OpenAI 官方服务通信。

<div align="center">
<br>
<sub>如果 OAIprism 对你有帮助，欢迎点一个 ⭐ 让更多人看到它。</sub>
</div>
