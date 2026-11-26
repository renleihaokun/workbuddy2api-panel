# API 端点

本文给客户端接入所需的稳定摘要。完整字段与行为差异见根 README 的 [API 端点](../README.md#-api-端点)、[Responses API](../README.md#responses-apicodex-直连)、[搜索端点](../README.md#搜索端点与余额刷新)。

## 鉴权

- `api_key` 非空：所有业务端点要求 `Authorization: Bearer <api_key>`。
- `api_key` 为空：**直接放行**。只适合完全隔离的本机环境。
- `/healthz` 恒无鉴权。
- 鉴权摘要常量时间比较；不要用空 key 的示例配置直接暴露端口。

## 端点总览

| 端点 | 方法 | 用途 |
|---|---|---|
| `/v1/chat/completions` | POST | OpenAI 兼容聊天补全；流式 / 非流式 |
| `/v1/responses` | POST | OpenAI Responses API；Codex CLI 直连 |
| `/v1/models` | GET | 模型目录、能力、上下文 / 输出上限、思考档位 |
| `/v1/search/messages` | POST | Anthropic Messages 形态的**搜索**端点 |
| `/status` | GET | 账号池状态、模型锁池等运维数据 |
| `/healthz` | GET | readiness（空池 / 全不可用返回 503） |
| `/panel/` | GET | 内嵌 Web 面板 |
| `/panel/api/*` | GET/POST | 面板自身接口，使用同一 Bearer 口径 |

### 明确不支持

- `/v1/messages` 不提供 Anthropic 模型对话。它是留给未来对话转发的路径；当前 `messages.go` 只实现 `/v1/search/messages` 搜索兼容壳。
- 不支持的内置工具 / 会话存储 / `previous_response_id` 等参数按 README 的 Responses 表处理；不要静默假设支持。

## 请求体限制

| 路径 | 上限 | 超限行为 |
|---|---|---|
| `/v1/chat/completions` | 64 MiB | 网关直接 `413 request_too_large` |
| `/v1/responses` | 64 MiB（共用同一安全网） | 同上 |
| `/v1/search/messages` | 1 MiB | 网关直接 413 |

这是进程内存安全网，不是可配置业务上限。多图 / 长上下文排障时先看实际字节数。

## Chat Completions

```bash
curl -sN http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer $WB2API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

- 上游出站强制 `stream:true`；网关负责非流式聚合。
- SSE 白名单重建，保证一个 `data: [DONE]`。
- 模型名支持 `cn:` / `global:` 前缀；请求侧也接受斜杠写法，展示写法由 `models.id_style` 决定。

## Responses API

`POST /v1/responses` 入站改写为 chat，出站把 JSON/SSE 翻回 Responses 事件。没有服务端会话存储：

- 每一轮必须回传完整 `input`；
- `store` 恒为 `false`；
- `previous_response_id` / `background` / `conversation` 等返回 `400 unsupported_parameter`；
- `namespace` 工具组会摊平成普通 function（Chat 侧 `<namespace>__<tool>`，默认 `functions` 命名空间不加前缀；点号/冒号等字符替换成 `_`，上游 11152 不接受）；响应里的 `function_call` / `custom_tool_call` 会回 `name` + `namespace`，Codex 按原命名空间路由；
- `web_search` 等内置工具无法由本网关执行，丢弃并记 WARN；
- Codex 配置：

```toml
model = "deepseek-v4-pro"
model_provider = "wb2api"

[model_providers.wb2api]
name = "WorkBuddy2API"
base_url = "http://127.0.0.1:7863/v1"
env_key = "WB2API_KEY"
wire_api = "responses"
```

`env_key` 是环境变量名，值填网关 `api_key`。

## 搜索端点

`POST /v1/search/messages` 只处理 web_search 搜索，不调用模型对话。客户端（如 DSH 搜索 provider）把搜索 baseURL 指到 `<网关>/v1/search`。

- `tools` 必须声明 web_search；
- 搜索结果在 `web_search_tool_result` block；
- 摘要走 `text.citations[].cited_text`；
- `allowed_domains` / `freshness` / `blocked_domains` 上游静默失效，因此本端点不承诺支持。

## 健康检查

```bash
curl -i http://127.0.0.1:7863/healthz
```

200 / 503 同结构：

```json
{"healthy":2,"total":3,"service":"workbuddy2api"}
```

- 200：至少一个 healthy 且未占满账号；
- 503：没有可用账号或健康判定不通过；
- 响应另带 `X-Service: workbuddy2api`。

用于严格身份校验时，用 `service == "workbuddy2api"`，不要只看 HTTP 2xx。该端点是 readiness，不是进程存活检测。