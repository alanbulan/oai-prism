# Yjs 依赖调研 + 沙箱同步方案

> 本文所有结论都来自**实际执行**（`go get` / `go mod tidy` / 编译运行 / 真实上游往返），
> 不是凭记忆或推断。凡是没有验证到的，都显式标注为「未验证」。

---

> **2026-10-04 更新**：同步沙箱仍然不需要 Yjs；但**附件**（用户发的图片）必须登记进项目的 Yjs
> 文档，沙箱才会把它落进工作区。为此实现了一个最小的纯 Go 编解码（`internal/ydoc`，只覆盖文件树
> 用到的 Y.Map 与基本值），经 Y-Sweet 的 HTTP 接口（`GET {baseUrl}/as-update`、`POST {baseUrl}/update`）
> 读写，不需要 WebSocket，也没有引入下文评估的依赖。详见[架构与原理 · 附件](架构与原理.md#附件图片)。

## 一、结论先行

两条结论，第二条推翻了原本的技术判断：

### 1. 不需要在 Go 里实现 Yjs / lib0 —— 整条链路是 5 步纯 HTTP

原本的判断是「无浏览器环境下跑通，必须实现 Yjs 同步协议客户端」。
**这个前提不成立。** 实测发现：沙箱拿到凭证后是**它自己**去连 Y-Sweet WebSocket
同步文档的，我们只负责把凭证送到。

用真实凭据跑通的完整链路（回答 `PONG`，HTTP 200，首次 14.2 s / 缓存命中 5.6 s）：

| # | 请求 | 端点 | 实测结果 |
|---|---|---|---|
| 1 | 建项目 | `POST /api/projects` | ✅ 返回真实 uuid |
| 2 | 申请沙箱 | `POST /api/backend/1/new` | ✅ `{url, token}` |
| 3 | 后端签发资源令牌 | `POST /api/projects/{id}/sandbox/resources-token` | ✅ `{access_token, resources_base_url, expires_at, max_age_seconds}` |
| 4 | 把令牌交给沙箱 | `POST <sandbox>/resources-token` | ✅ `{"status":"success"}` |
| 5 | 取 Y-Sweet 凭证 | `POST /api/y` | ✅ `{docId, url(wss), baseUrl, authorization, token}` |
| 6 | 原样转交沙箱 | `POST <sandbox>/token` | ✅ `{"success":true,"message":"Token received"}` |
| 7 | 等就绪 | `GET <sandbox>/wait-for-sync?wait_ms=10000` | ✅ 约 3 秒后 `status=synced` |
| 8 | 发起生成 | `POST /api/llm/response_with_tools_start` | ✅ `request_id` + `turn_state` |
| 9 | 轮询 | `POST /api/llm/response_with_tools_status` | ✅ `completed` + 正文 |

**所以：沙箱方案不存在「自己实现 lib0」这个工作项。** 第 6 步之后，
沙箱内部自行完成文档同步。

### 2. 若将来真需要 Yjs，Go 生态里可用的是 `github.com/reearth/ygo`

只增加 **1 个**依赖，且能编译运行（详见第三节）。

---

## 二、调研方法

不靠记忆判断，全部实测：

```bash
mkdir probe && cd probe && cat > go.mod <<'EOF'
module goprobe
go 1.25
EOF
go get <candidate>@latest      # 看能否下载、拉进哪些依赖
cat go.mod
```

再用 `go doc <pkg>` 读公开 API、读模块源码确认能力，
最后写最小程序 **编译 + 运行** 验证功能真的可用。

---

## 三、候选对比（全部实测）

| 候选 | 版本 | `go get` | go.mod 新增依赖 | 判定 |
|---|---|---|---|---|
| **`github.com/reearth/ygo`** | **v1.50.0** | ✅ | **0 个**（只用 `crdt`+`sync` 时） | ✅ **推荐** |
| `github.com/haowjy/y-crdt` | v0.0.2 | ✅ | +2（`copystructure`、`reflectwalk`） | ⚠️ 版本极早期（v0.0.2） |
| `github.com/savaki/y-crdt` | — | ❌ | — | ❌ **不可用**：模块路径声明不符 |
| `github.com/dbesio/ygo` | — | — | 需 **cgo + Rust yffi** | ❌ 违反本项目零 CGO 约束 |
| `github.com/CFTL/YGS` | — | — | — | ❌ 单次提交、已停更 |
| `y-crdt/y-crdt`（Rust 系） | — | — | `yffi` cgo 绑定 | ❌ 同上 |

### 3.1 `github.com/reearth/ygo` 细节

**它的 go.mod 声明了 6 个直接依赖**：

```
github.com/alicebob/miniredis/v2
github.com/gorilla/websocket
github.com/redis/go-redis/v9
github.com/stretchr/testify
golang.org/x/sync
modernc.org/sqlite
```

**但 Go 只编译被 import 的包。** 只 import `crdt` + `sync` 时，实测结果：

```
$ go mod tidy
$ cat go.mod
module minimal

go 1.25

require github.com/reearth/ygo v1.50.0      ← 就这一个
```

`redis` / `sqlite` / `websocket` 全部没被拉进构建。
（`go.sum` 里会有若干条校验和，那是模块图的完整记录，不进二进制。）

**编译运行验证过的最小可用代码**：

```go
import (
    "github.com/reearth/ygo/crdt"
    ygsync "github.com/reearth/ygo/sync"
)

a := crdt.New()
texA := a.GetText("main.tex")              // 注意：必须在 Transact 之外取
a.Transact(func(t *crdt.Transaction) {
    texA.Insert(t, 0, `\documentclass{article}`, nil)
})

// y-protocols 握手（就是 Y-Sweet 服务端说的话）
step1 := ygsync.EncodeSyncStep1(a)
step2, _ := ygsync.EncodeSyncStep2(b, step1)
reply, _ := ygsync.ApplySyncMessage(b, step2, nil)

// v1 增量编码（WebSocket 上真正传输的格式）
upd := a.EncodeStateAsUpdate()
_ = crdt.New().ApplyUpdate(upd)
```

验证通过：`crdt.New` / `GetText` / `Transact` / `YText.Insert` /
`EncodeStateAsUpdate` / `ApplyUpdate` / `sync.EncodeSyncStep1` /
`EncodeSyncStep2` / `ApplySyncMessage` 全部按预期工作。

**按需再引的能力**（用到哪个才付哪个的依赖代价）：

| 包 | 附加依赖 | 用途 |
|---|---|---|
| `provider/websocket` | `gorilla/websocket` | WebSocket 服务端/客户端 provider |
| `provider/client` | 同上 | 连远端 Yjs 服务的客户端（含 auth 帧、重连、awareness） |
| `provider/http` | 无 | HTTP 拉推式同步 |
| `persistence/sqlite` | `modernc.org/sqlite`（**很重**） | 版本化持久化 |
| `cluster/redis` | `go-redis/v9` | 跨进程集群 |

协议实现有官方兼容性测试夹具（`sync/testdata`、`crdt/testdata`），
README 明确对标 `yjs/y-protocols` 的 PROTOCOL.md。

### 3.2 为什么其余候选不可用（实测细节）

**`github.com/savaki/y-crdt`** —— `go get` 直接失败：

```
go: github.com/savaki/y-crdt@latest (...): parsing go.mod:
	module declares its path as: github.com/skyterra/y-crdt
	        but was required as: github.com/savaki/y-crdt
```

模块路径声明与实际仓库不符，Go 工具链直接拒绝。**不可用**（除非用
`replace` 硬指到 `skyterra/y-crdt`，但那是在用一个连自己路径都写错的包）。

**`github.com/haowjy/y-crdt`** —— 能装，但引入 2 个额外依赖
（`mitchellh/copystructure`、`mitchellh/reflectwalk`），且版本是 `v0.0.2`。
作者自述「Encoder/decoder v2 support is pending development」。
作为备选可以，但不优于 ygo。

**`github.com/dbesio/ygo`** —— 是 `y-crdt` 官方 `yffi` 的 cgo 封装，
使用它必须引入 Rust 工具链与 CGO。本项目硬约束是「零 CGO、单二进制」，
直接出局。

---

## 四、为什么不需要它：沙箱自行同步的证据

### 4.1 状态机是自证的

`wait-for-sync` 返回的是一份结构化的「我还缺什么」清单：

```jsonc
// 只申请沙箱、不做任何注入时（status 永远停在 syncing）
{"status":"syncing","readinessCapabilities":["current_y_sweet_provider"],
 "tokens":{"hasResourceToken":false,"hasResourceBaseUrl":false,
           "hasResourceProjectId":false,"hasCurrentYSweetToken":false,
           "hasSyncedYSweetProvider":false,"fileCredentialSource":"none"}}

// 注入资源令牌之后（projId 与 credSrc 变了，但还缺 Y-Sweet）
{"status":"syncing", ... "hasResourceProjectId":true,
                       "fileCredentialSource":"resources-token", ...}

// 交付 Y-Sweet 凭证之后 —— 1 秒内变成
{"status":"synced", ... "hasCurrentYSweetToken":true,
                       "hasSyncedYSweetProvider":true, ...}
```

**最后一步的转变是关键证据**：`hasSyncedYSweetProvider` 由 false 变 true，
说明是**沙箱自己**完成了 provider 同步。我们只上交了 `{token, url}`，
一行 Yjs 代码都没写。

### 4.2 凭证的内容也印证了这一点

`POST /api/y` 返回的是 **WebSocket 地址**：

```json
{"docId": "<project_uuid>",
 "url":   "wss://prism.openai.com/y/d/<uuid>/ws",
 "baseUrl":"https://prism.openai.com/y/d/<uuid>",
 "authorization": "full",
 "token": "ASQ2ZDU0YWQ2Mi0..."}
```

拿到 `wss://` 地址的是**要主动连出去的那一方** —— 即沙箱。
我们只是信使。

---

## 五、真实踩过的坑（全部实测确认）

### 5.1 沙箱认证是**双重**的

只带 `X-Crixet-Sandbox-Token` 会得到 **401 且响应体为空**，极难排查。
必须同时带 Cookie（会话）。

```python
# ❌ 401
headers = {"X-Crixet-Sandbox-Token": sb_token}

# ✅ 200 {"status":"success"}
headers = {"X-Crixet-Sandbox-Token": sb_token, "cookie": cookie}
```

原因：前端 `fetchSandboxWithRetry` 默认 `credentials:"same-origin"`，一直都带着 Cookie。

### 5.2 沙箱 URL 不能当绝对 URL 直接发

沙箱 URL 是 `https://prism.openai.com/s/sandboxes/proxy/`（绝对地址），
但我们的 HTTP 客户端是把 path **拼接**到 BaseURL 上的。
直接传绝对 URL 会拼出 `/https://prism.openai.com/...` 这种畸形路径，
服务端回 404 —— 症状是「端点不存在」，极难联想到 URL 拼接。

**正确做法**：只取 path（`/s/sandboxes/proxy/`）再拼子路径，复用同一个客户端
（连接池、HTTP/2、Cookie 注入全在里面）。已加回归测试。

### 5.3 `404` / `501` 是**成功**语义

真实前端的等待循环里：

```javascript
if (404===o.status || 501===o.status) return {ok:!0};   // 视为"无需同步"
```

这两个状态码表示「这个沙箱不需要工作区同步」，不是错误。

### 5.4 资源令牌**绑定单个项目**，且只有 1 小时

JWT claims 里带着 `project_uuid`：

```json
{"iss":"crixet.sandbox.resources","aud":"crixet.sandbox",
 "crixet_token_kind":"sandbox_resources",
 "project_uuid":"23cca5e7-...", "exp":1789609321}
```

所以缓存粒度必须是 **(账号, 项目)** 而不是账号级。
按账号缓存会让第二个项目拿到「已同步」的假信号 → 固定 122 秒后 504。

### 5.5 「固定 122 秒后 504」是同步缺失的特征值

两次不同项目、两次都是 **122.x 秒**，说明这是服务端固定超时，
而不是「容器冷启动慢」。看到这个数字就该去查 `wait-for-sync` 的 `tokens` 字段。

### 5.6 「上游维护中」曾是误判

早期拿到的 `503 + "We're investigating reports that users are not able to
compile and use AI features"` 被当成上游维护窗口。
**实际是 Cloudflare 对不带完整浏览器头的裸请求做的机器人拦截。**
带上 `User-Agent` / `Origin` / `Referer` / `Cookie` 后，`GET /api/maintenance`
返回 `{"mode":"off"}`。

---

## 六、实测性能数据

| 场景 | 延迟 | 说明 |
|---|---|---|
| 首次请求（建项目 + 申请沙箱 + 同步 + 生成） | **14.2 s** | 一次性开销 |
| 相同会话再次请求（全缓存命中） | **5.6 s** | 项目命中 + 沙箱同步跳过 |
| 沙箱工作区同步 | **2.6 – 3.3 s** | 四步注入 + 等就绪 |

指标印证（4 次请求后）：

```
oaiprism_project_ops_total{op="create",status="ok"}      3
oaiprism_project_ops_total{op="reuse",status="hit"}      1
oaiprism_sandbox_ops_total{op="acquire",status="ok"}     1
oaiprism_sandbox_ops_total{op="acquire",status="hit"}    3
oaiprism_sandbox_ops_total{op="sync",status="ok"}        3   ← 3 个项目各同步一次
oaiprism_sandbox_ops_total{op="sync",status="hit"}       1   ← 第 4 次未重复同步
```

项目复用键是「system 提示 + 首条 user 消息」的哈希；客户端也可以用
`X-Oaiprism-Session` 请求头显式指定会话，获得最稳定的复用。

---

## 七、未验证 / 待确认

诚实列出没验证到的：

1. **Y-Sweet 文档里到底存什么结构** —— 沙箱自行同步，我们从没读过文档内容。
   如果将来要「代理侧主动写入 LaTeX 内容」，就必须真用 Yjs 了（届时选 ygo）。
2. **`sandbox_session_id` 的作用** —— 实测 `/api/backend/1/new` 目前**不返回**
   这个字段（响应只有 `{url, token}`），但签发资源令牌时它是合法入参。
   代码已兼容缺失（传 `null`）。
3. **资源令牌过期后的行为** —— 缓存按 `expires_at` 主动失效，但没有实测过
   「用过期令牌请求沙箱会得到什么错误」。
4. **`pending` 帧是否携带正文** —— 与本主题无关但同属待确认：真实前端
   固定 5 秒轮询且 pending 分支不读 `response`，暗示不带。两种形态都已支持。

---

## 八、实施位置（Go 侧）

| 文件 | 内容 |
|---|---|
| `internal/prism/types.go` | `PathResourceToken` / `PathYSweetToken` / `ResourceToken` / `YSweetToken` / `SandboxSyncStatus` |
| `internal/prism/client.go` | `AcquireResourceToken` / `DeliverResourceToken` / `AcquireYSweetToken` / `DeliverYSweetToken` / `WaitSandboxReady`（含 `sandboxPath` 归一化） |
| `internal/facade/sandbox.go` | 按 (账号, 项目) 粒度的同步状态缓存 |
| `internal/facade/runner.go` | `syncSandboxWorkspace` 编排四步 + 等就绪 |
| `configs/config.example.yaml` | `use_sandbox` / `sandbox_ttl` / `sandbox_ready_wait` |

**新增第三方依赖：0 个。** `go.mod` 依然只有 `gopkg.in/yaml.v3`。
