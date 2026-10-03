# OAIprism

把 OpenAI Prism（`prism.openai.com`）的内部推理协议包装成标准接口的高性能网关。

- **标准接口**：OpenAI Chat Completions / Responses、Anthropic Messages，官方 SDK 与 Codex CLI 直接可用；
- **Codex 工具桥**：上游负责思考，本地 Codex CLI 负责执行，文件真实落在你的磁盘上；
- **多账号池**：调度、粘性、冷却、凭据自动续期、热重载；
- **Dashboard**：账号管理、API Key 管理、请求流水与统计、对话调试台；
- **原样反代**：不理解协议也能用的保底通道（白名单 + 凭据注入）。

Go 单二进制，零 CGO，5 个直接依赖。

---

## 目录

- [系统架构](#系统架构)
- [快速开始](#快速开始)
- [鉴权与 API Key](#鉴权与-api-key)
- [接入客户端](#接入客户端)
- [端点一览](#端点一览)
- [多轮上下文](#多轮上下文)
- [Codex 工具桥](#codex-工具桥)
- [上游协议（已实测校准）](#上游协议已实测校准)
- [沙箱](#沙箱)
- [安全设计](#安全设计)
- [性能设计](#性能设计)
- [运维](#运维)
- [开发与 CI](#开发与-ci)
- [目录结构](#目录结构)

---

## 系统架构

```
                              ┌──────────────────────────────────────────────┐
   OpenAI SDK / Codex CLI ──► │  8787  OAIprism 网关（oaiprism serve）        │
   Anthropic SDK          ──► │  ├─ /v1/*          OpenAI / Anthropic 门面    │
   curl / 任意 HTTP 客户端 ──► │  ├─ /prism/*       原样反代（白名单）          │
                              │  ├─ /admin/*       管理 API                   │
                              │  ├─ /dashboard/    Dashboard（web/dist）       │
                              │  └─ /healthz /readyz /metrics                  │
                              └───────────────┬──────────────────────────────┘
                                              │ 调优 HTTP 客户端（HTTP/2、连接预热）
                                              ▼
                              ┌──────────────────────────────────────────────┐
                              │  8790  Go TLS 桥（oaiprism tlsbridge）        │
                              │  Chrome 系 TLS 指纹传输；每请求向 8791 取      │
                              │  一次性 Sentinel token                         │
                              └───────────────┬──────────────────────────────┘
                                              │ 带 openai-sentinel-token
                                              ▼
                              ┌──────────────────────────────────────────────┐
                              │  8791  Sentinel token oracle（node + Chrome） │
                              │  在真实页面里执行 SentinelSDK.token()，只签发  │
                              └───────────────┬──────────────────────────────┘
                                              ▼
                                   prism.openai.com（Cloudflare 之后）
```

**一次推理**：客户端 → 8787（协议翻译为上游 start + 轮询）→ 8790（补 Sentinel token、
按浏览器指纹发出）→ 上游 → 8787 把轮询结果转成增量 SSE 推回客户端。

**为什么需要桥**：Cloudflare 拦截不具备浏览器信任特征的请求，写操作还需要
Sentinel 反自动化令牌。8790 解决"传输指纹"，8791 只负责"签发令牌"，
两者拆开后各自崩溃面小、可独立重启。

| 端口 | 服务 | 启动方式 | 说明 |
|---|---|---|---|
| **8787** | 网关 | `oaiprism serve` | 对外唯一入口（API + Dashboard + 探针） |
| **8790** | TLS 桥 | `oaiprism tlsbridge` | 仅本机监听；网关的 `upstream.base_url` 指向它 |
| **8791** | Sentinel oracle | `node tools/sentinel_oracle.js` | 仅本机监听；`POST /token`、`GET /healthz` |

---

## 快速开始

### 1. 编译

```bash
go build -o oaiprism.exe ./cmd/oaiprism
```

### 2. 准备账号凭据

```bash
# 浏览器开发者工具里复制 prism.openai.com 的整串 Cookie：
./oaiprism.exe import -cookie "prism_oai_access_token=eyJ...; prism_session_token=..." -id main

# 或者只给 access token（Prism 的 JWT，约 10 天有效）：
./oaiprism.exe import -access-token "eyJhbGci..." -id main

# 有 refresh_token 最省心（可自动续期）：
./oaiprism.exe import -refresh-token "rt.1..." -id main

# 从标准输入读取，避免进入 shell 历史：
./oaiprism.exe import -stdin -id main
```

`import` 会在线校验凭据、补全 email/plan/account_id、尽可能换取 refresh_token，
然后写入 `secrets/accounts.json`（`0600` 权限）。也可以启动后在 Dashboard
「账号」页走**官方 OAuth 授权导入**，或直接编辑该文件 —— 运行中保存即在 5 秒内热加载。

> Prism 用的是自家 cookie（`prism_oai_access_token` / `prism_session_token`），
> 不是 next-auth 的 `__Secure-next-auth.session-token`。

### 3. 启动

三个服务按 **oracle → 桥 → 网关** 的顺序启动。

```powershell
tools\start_bridge.ps1     # 一键启动（检查端口 → 构建 → 依次拉起 → 健康检查）
tools\stop_bridge.ps1      # 一键停止（含自动化 Chrome 清理）
tools\check_bridge.cmd     # Codex 报错时的诊断脚本
```

手动启动（等价，便于排障）：

```bash
node tools/sentinel_oracle.js auto 8791      # 会打开一个 Chrome 窗口，属正常
./oaiprism.exe tlsbridge -port 8790 -oracle http://127.0.0.1:8791 -accounts secrets/accounts.json
cp configs/config.example.yaml configs/config.yaml
./oaiprism.exe serve -config configs/config.yaml
```

日志：脚本启动时在 `%TEMP%`（`oracle.log` / `tlsbridge.log` / `oaiprism_8787.log`）。
脚本头部的仓库与 node 路径是本机约定，换机器时修改即可。

> 只做离线调试时可以只起网关：只读端点可用，需要 Sentinel 的写操作会 403；
> 完全离线可把上游指向 `tools/mock_upstream.py`。没配凭据也能启动，`/readyz` 会返回 503 提示。

### 4. 创建 API Key 并调用

首次启动时没有任何 Key，网关**只接受本机请求**。打开
`http://127.0.0.1:8787/dashboard/`，在右上角「API 密钥」里生成一个 Key（生成后自动保存到
本浏览器）。此后所有请求都必须携带有效 Key，详见[鉴权与 API Key](#鉴权与-api-key)。

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer $OAIPRISM_KEY" -H "Content-Type: application/json" \
  -d '{"model":"gpt-6.1-sol","stream":true,"messages":[{"role":"user","content":"你好"}]}'
```

---

## 鉴权与 API Key

**有效 Key = 配置文件 `facade.api_keys` ∪ Dashboard 生成的 Key（存于 SQLite）。**

| 场景 | 行为 |
|---|---|
| 两者都为空 | 只放行**本机**请求（对端为回环地址，且 `Host` 是 localhost 或 IP 字面量） |
| 存在任意 Key | 所有接口都必须带有效 Key（`Authorization: Bearer`、`x-api-key` 或 `api-key` 头） |
| 豁免 | `/healthz`、`/readyz`、`/metrics`、`/dashboard/` 静态资源、`/admin/login`、OAuth 回调 |

管理端（`/admin/*`）权限更高，满足其一即可：

1. **本机请求** + 任一有效 Key；
2. 配置文件 `facade.api_keys` 中的 Key（运维显式下发，可远程管理）；
3. 管理员登录令牌：配置 `server.admin_password`（或环境变量 `OAI_PRISM_ADMIN_PASSWORD`）后，
   `POST /admin/login` 签发 12 小时有效的会话令牌。未配置密码时不开放密码登录。

Dashboard 生成的普通 Key **只能调用 API**，远程持有者不能管理账号或签发 Key。
管理端写操作会校验 `Origin`，挡住浏览器跨站请求。

Key 由 `crypto/rand` 生成（`sk-prism-` + 32 位十六进制），签发与注销即时生效。
会话状态按 Key 的指纹隔离：不同 Key 的调用方即使会话标识相同也互不可见。

> 早期版本会自动种入固定 Key `sk-prism-live-master`，它写在源码里、等于公开。
> 若仍在使用，网关启动时会告警：请生成新 Key、更新客户端配置后注销它。

---

## 接入客户端

**OpenAI SDK**

```bash
export OPENAI_BASE_URL=http://127.0.0.1:8787/v1
export OPENAI_API_KEY=<你的 Key>
```

**Anthropic SDK**

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export ANTHROPIC_API_KEY=<你的 Key>
```

**Codex CLI**（`~/.codex/config.toml`）

```toml
model_provider = "oaiprism"
model = "gpt-6.1-sol"

[model_providers.oaiprism]
name = "oaiprism"
wire_api = "responses"
requires_openai_auth = false
base_url = "http://127.0.0.1:8787/v1"
experimental_bearer_token = "<你的 Key>"
```

Codex 每轮回传完整历史，网关会把它折叠成上游可读的上下文（见下节）。
上游单条消息上限约 15–16k tokens，建议同时设置 `model_context_window = 16384`，
让 Codex 在接近上限前自动压缩历史。

**可用模型**（`GET /v1/models` 为准）：`gpt-6.1-sol`（默认，另有 `-low` / `-high` / `-xhigh`）、
`gpt-6-luna`（`-high` / `-xhigh`）、`gpt-5.6-sol`（`-low` / `-high` / `-xhigh`）、
`gpt-5.6-terra`（`-high` / `-xhigh`）。映射表在 `facade.models`，可按需增删。

---

## 端点一览

**推理门面**（另有 `/chat/completions`、`/responses`、`/models` 无前缀别名）：

| 端点 | 说明 |
|---|---|
| `POST /v1/chat/completions` | Chat Completions（流式/非流式、图片输入） |
| `POST /v1/completions` | 老式 Completions，内部转 chat |
| `POST /v1/responses` | Responses API（新版 SDK / Codex CLI 首选，含工具桥） |
| `POST /v1/messages` | Anthropic Messages（完整流式事件序列） |
| `GET /v1/models`、`GET /v1/models/{id}` | 模型清单 |
| `POST /v1/embeddings`、`POST /v1/images/generations` | 明确返回 501：上游只提供对话式补全 |

**运维与探针**：`GET /healthz`（存活，不查下游）、`GET /readyz`（无可用账号时 503）、
`GET /metrics`（Prometheus 文本）、`GET /dashboard/`。

**管理 API**（`/admin/*`，权限见上节）：

| 端点 | 说明 |
|---|---|
| `GET/POST /admin/accounts`、`PUT/DELETE /admin/accounts/{id}` | 账号查询与增删改（不返回凭据内容） |
| `POST /admin/accounts/{id}/refresh`、`POST /admin/reload` | 强制刷新 token、重载凭据文件 |
| `POST /admin/oauth/begin`、`GET /admin/oauth/status`、`POST /admin/oauth/exchange` | 官方 OAuth 授权导入（PKCE） |
| `GET /admin/requests`、`GET /admin/statistics`、`GET /admin/stats` | 请求流水、调用统计、运行态 |
| `GET/POST /admin/apikeys`、`DELETE /admin/apikeys/{key}` | API Key 管理 |
| `/admin/chat/sessions…` | Dashboard 调试台会话持久化 |
| `POST /admin/login`、`GET /admin/me` | 管理员登录与状态 |

**原样反代**：`/prism/*` 按 `raw_proxy.allow_paths` 白名单（按路径段匹配）转发到上游，
注入账号池凭据。

### 请求级控制头

| 头 | 作用 |
|---|---|
| `X-Oaiprism-Session` | 会话身份：决定账号粘性、项目复用与会话历史 |
| `X-Oaiprism-Account` | 指定账号（调试用） |
| `X-Oaiprism-Project` | 指定上游项目 ID |
| `X-Oaiprism-Model` / `X-Oaiprism-Effort` | 覆盖模型名 / 推理强度 |
| `X-Oaiprism-Previous` | 显式传入上一轮 response id |
| `X-Local-Workspace` | 把上游产物写到**网关所在机器**的目录；默认关闭，需开启 `facade.local_workspace_write` 且仅限本机请求 |

---

## 多轮上下文

### 结论（均为真实请求对照实验所得）

1. **上游单次请求只处理「首条 system + 最后一条 user」**，中间的 input 条目会被丢弃；
2. **上游不会凭 `previousResponseId` 替我们拼接历史**（2026-10-03 实测：只发本轮增量 +
   上一轮句柄，第二轮必然失忆）；
3. 把历史折叠成 `[Previous Conversation History]` 文本并入首条 system，多轮记忆稳定可靠。

### 网关的做法

**每一轮都发送完整上下文**：

- 客户端自带历史（Codex CLI、标准 Chat 客户端）：历史折叠进首条 system；
- 客户端只发本轮消息：用网关的**会话链**（`internal/facade/session_chain.go`，
  最多 20 条 / 24k 字符，超限从最旧处裁剪）注入历史。

会话链的身份来自**强会话标识**：`X-Oaiprism-Session`、Codex 的 `client_metadata` /
`prompt_cache_key`、请求体里的 `conversation_id` 等；Responses API 客户端也可以用标准的
`previous_response_id` 续接。首条消息指纹、`user` 字段这类**弱标识**只用于账号/项目亲和，
绝不据此注入历史 —— 两段以同一句话开头的对话会撞到同一个弱标识。

项目（工作区）在同一会话内复用；`facade.upstream_continuation`（默认关闭）决定是否额外把
上游会话句柄与沙箱快照（`conversationId` / `previousResponseId` / `codex_listen_snapshot`）
一并发给上游。若携带的句柄已失效，网关会丢弃句柄、用完整上下文原地重试一次。

### 上游的窗口与原生续接

- conversation 窗口约 **2M tokens**（浏览器内同一 conversation 连续 88 轮，1.92M tokens
  仍记得暗号）；单条消息上限约 **15–16k tokens**，超限报错
  `This request is too large to send...`；
- 真实浏览器内的原生续接（同一 `conversationId` + 上一轮 `resp_*` + `codex_listen_snapshot`
  + 仅本轮增量）可以成功，但同样的请求经 Go 通道发出会失忆 —— 上游把非浏览器级信任的请求
  当作无状态会话。`tools/browser_forward.js`（浏览器全请求代发）是这一方向的实验件。

数据与脚本：`tools/webui_probe*.js`、`tools/webui_probe7_log.jsonl`、`tools/probe_context_limit.py`。

---

## Codex 工具桥

上游是 server-side tools 架构：模型的终端/文件工具在**上游容器**里执行，客户端只拿到最终
文本 —— 本地 Codex CLI 的工具一次都不会被调用，"创建"的文件也不在你的磁盘上。

工具桥反过来：在 system 指令里告诉模型"没有执行环境，所有操作必须输出一个
```` ```codex-exec ```` 代码块"；网关把代码块翻译成 Responses 协议的工具调用
（`custom_tool_call` 或 `function_call`，按 CLI 实际注册的工具类型与名字），Codex CLI
在本地执行并回传结果，网关再把结果翻译回上下文。工具声明的两条路径（input 内
`additional_tools`、顶层 `tools`）都能识别。

若模型没有输出代码块、却在上游容器里改了文件（DeltaFiles），网关会把变更合成为本地命令：

- **新增文件**按完整内容写入（Base64 传递，无转义问题；Windows 大文件分块，规避
  32,767 字符的命令行上限）；
- **修改**一律按"在本地文件中定位原文 → 替换"落地，保留原换行风格与 BOM；
  定位不到就报错、**不改文件**；
- 路径必须是工作区内的相对路径，并按 PowerShell / POSIX shell 规则转义。

---

## 上游协议（已实测校准）

> 详见 [`docs/协议校准报告.md`](docs/协议校准报告.md) 与 [`docs/Prism完整调用链.md`](docs/Prism完整调用链.md)。

推理端点：`/api/llm/response_with_tools_{start,status,stop}`。

```jsonc
// POST /api/llm/response_with_tools_start
{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"…"}]}],
 "metadata":{"model":"gpt-6.1-sol",          // 模型参数在 metadata 里，不在顶层
             "reasoning_effort":"medium",
             "projectId":"…",
             "frontend_origin":"https://prism.openai.com"}}
// → {"status":"started","request_id":"…","turn_state":{…}}        需要轮询
// → {"status":"completed","request_id":"…","response":{…}}        已经结束

// POST /api/llm/response_with_tools_status   必须带 {request_id, turn_state}
// → {"status":"pending","turn_state":{…新令牌…}}
// → {"status":"completed","response":{"status":"success","payload":{"output":[…]}}}
// 答案在 response.payload.output[-1].content[*].text（只取最后一条，多轮下会累积）
```

三个反直觉的点：

1. **失败是 HTTP 200**：`status:"completed"` + `response.status:"error"`，只看状态码会得到空回答；
2. **`turn_state` 是续令牌**，每轮换新且必须原样回传，自造值会被拒；
3. **大小写不统一**：start 用 `conversationId`，status 用 `request_id`，不要"顺手统一"。

`start` 不是幂等操作：网关只对确定未送达的失败（建连失败、429/503）重试，避免在同一会话上
重复发起生成。字段名都可通过 `facade.schema` 配置，上游变化时不必重新编译。

---

## 沙箱

> 详见 [`docs/Yjs依赖调研与沙箱方案.md`](docs/Yjs依赖调研与沙箱方案.md)。

上游的 AI 在容器沙箱里工作，并且需要项目工作区。网关自动完成全部前置步骤：

```
1. POST /api/backend/1/new                          申请沙箱 → {url, token}
2. POST /api/projects/{id}/sandbox/resources-token  签发资源令牌（绑定项目，1 小时）
3. POST <sandbox>/resources-token                   交给沙箱
4. POST /api/y                                      取 Y-Sweet 文档凭证
5. POST <sandbox>/token                             原样交给沙箱
6. GET  <sandbox>/wait-for-sync                     等待 status=synced
```

第 5 步之后是沙箱自己同步文档，网关不需要实现 Yjs。漏掉任何一步，沙箱都不报错，只会一直停在
`syncing`，表现为会话**固定 122 秒**后 504（`Please submit prompt again.`）。

- 认证是双重的：`X-Crixet-Sandbox-Token` + Cookie，只带前者会得到空响应体的 401；
- 沙箱按账号缓存，工作区同步状态按（账号, 项目）缓存，过期点与资源令牌一致；
- 冷启动未就绪时原地退避重试，只有明确断连或连续 5 次未就绪才重建容器；
- 实测延迟：首次（建项目 + 申请沙箱 + 同步 + 生成）约 14 s，同会话后续请求约 5–6 s。

---

## 安全设计

- **鉴权默认收紧**：没有任何 Key 时只放行本机；管理端与 API 权限分离（见上文）。
- **不读取调用方指定的本地文件**：`image_url` 只接受 `data:image/*` 与解析到公网地址的
  http(s) 链接，建连时二次校验地址（防 DNS rebinding），且内容必须真的是图片。
- **上游产物落地受控**：只写工作区内相对路径；修改不做整体覆盖；`X-Local-Workspace` 默认关闭。
- **原样反代**：丢弃调用方的 `Authorization` / `Cookie`，绝不回传上游 `Set-Cookie`，
  白名单按路径段匹配。
- **会话隔离**：会话链按调用方 Key 指纹隔离，弱会话标识不继承上下文。
- **凭据**：`/admin/accounts` 不返回凭据内容；凭据文件 `0600` 写入；请求流水记录的账号与
  客户端 IP 不信任客户端可伪造的请求头。
- **OAuth 回调**：`state` 必须精确匹配。

---

## 性能设计

- **连接**：`MaxIdleConnsPerHost: 256`、启动预热 TCP/TLS、出站强制 HTTP/2、关闭自动 gzip；
- **流式**：SSE 缓冲来自 `sync.Pool`，手写 JSON 转义，每个增量立即 `Flush`，
  并设置 `X-Accel-Buffering: no`（否则 Nginx 会把整段流缓冲到结束）；
- **轮询转流式**：无条件节流（不会退化成忙轮询）、空轮询自适应退避、前缀差分还原 token 级增量；
  等待上游期间持续发送心跳，避免中间链路按空闲断开；
- **项目复用**：建项目比推理本身还慢，同一会话只建一次；无会话标识的请求轮转分桶；
- **账号粘性**：命中即续期；粘性账号仅因并发已满时短暂等待，而不是立刻换号；
- **指标**：`oaiprism_facade_first_delta_seconds` 直接衡量首字延迟。

---

## 运维

```bash
./oaiprism.exe probe -config configs/config.yaml                    # 检查账号是否可用
curl -H "Authorization: Bearer $KEY" http://127.0.0.1:8787/admin/accounts   # 账号池运行态
curl -s http://127.0.0.1:8787/metrics | grep -E 'first_delta|facade_runs|poll_rounds'
```

常用配置（完整说明见 `configs/config.example.yaml`）：

| 配置 | 说明 |
|---|---|
| `facade.api_keys` | 配置文件 Key（同时具备管理权限）；也可用 `OAI_PRISM_API_KEYS` |
| `server.admin_password` | Dashboard 管理员密码；也可用 `OAI_PRISM_ADMIN_PASSWORD` |
| `facade.upstream_continuation` | 是否额外附带上游续接句柄（默认 `false`） |
| `facade.local_workspace_write` | 是否允许 `X-Local-Workspace` 写网关本机目录（默认 `false`） |
| `facade.reuse_project` / `project_ttl` | 项目复用 |
| `facade.use_sandbox` / `sandbox_ready_wait` | 沙箱前置流程 |
| `pool.strategy` / `sticky_ttl` / `max_wait` | 账号调度、粘性、全池冷却时的最长等待 |
| `raw_proxy.allow_paths` | 原样反代白名单 |

**部署**：单二进制 + `web/dist` 静态资源，发布即替换文件后重启网关（`make build-linux`
产出 `bin/oaiprism-linux-amd64`）。桥与 oracle 依赖浏览器登录态，属于运行环境，日常发布不动。
SQLite（`secrets/accounts.db`）向前兼容，回滚只需换回上一版二进制。

---

## 开发与 CI

提交前本地门禁（与 CI 完全一致）：

```bash
go vet ./... && go test ./... && go test -race ./...
cd web && npx tsc --noEmit && pnpm build
```

`-race` 不可省略：账号池、粘性表、SSE 写出、会话链、热重载都是并发结构。

CI（`.github/workflows/ci.yml`，任意分支 push / PR 触发）：

- **go-check**：vet → test → race → 本机构建 → 交叉编译 linux/amd64、linux/arm64 → 上传产物；
- **web-check**：`pnpm install --frozen-lockfile` → `tsc --noEmit` → `pnpm build`。

测试包含**模拟上游**的端到端用例（`internal/server/e2e_test.go`），以及会在 pwsh、
Windows PowerShell 与 bash + python3 中**真实执行**合成补丁脚本的回归测试
（`internal/facade/regression_test.go`），逐字节核对落盘结果。

---

## 目录结构

```
cmd/oaiprism/        命令行入口（serve / tlsbridge / probe / import / capture-summary）
internal/
  config/            配置加载与校验
  creds/             凭据建模、JWT 解析、自动续期（session + OAuth）
  httpc/             面向上游的调优 HTTP 客户端
  account/           账号池：调度、粘性、并发闸门、冷却、热重载、SQLite 持久化
  prism/             上游协议客户端、宽容解析、前缀差分
  sse/               SSE 写出层
  facade/            兼容门面：Chat / Responses / Anthropic、会话链、工具桥、文件变更落地
  bridge/            Go TLS 桥（浏览器指纹传输 + Sentinel token 编排）
  rawproxy/          原样反代通道
  capture/           抓包录制与协议摘要
  middleware/        恢复、请求 ID、指标、鉴权、限流
  metrics/           零依赖 Prometheus 指标
  server/            组件装配、管理 API、鉴权接线、生命周期
web/                 Dashboard（React 19 + antd v6），产出 web/dist
tools/
  start_bridge.ps1 / .cmd、stop_bridge.ps1、check_bridge.cmd   一键启停与诊断
  sentinel_oracle.js  Sentinel token oracle（8791）
  browser_forward.js  浏览器全请求代发（实验件）
  probe_* / test_* / verify_*   协议探测与真实链路验证脚本
  webui_probe*        真实浏览器协议考古（多轮续接、窗口上限）
  sentinel/           Sentinel 逆向资产
configs/             config.example.yaml（入库）；config.yaml（本地，不入库）
docs/                协议校准报告、完整调用链、部署报告、沙箱方案
```
