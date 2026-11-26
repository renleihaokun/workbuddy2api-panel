# 同类项目调研：可借鉴清单

> 调研时间：2026-11-11　｜　样本：GitHub 公开仓库 README / 官方文档　｜　star 数为检索时快照，只作热度参考
>
> **状态说明（2026-11-26）**：本文是规划与竞品快照，不是现行接口清单；其中“现状 / 缺口”按调研时点记录。当前对外的模型对话协议、配置字段与行为以仓库根目录 README 和代码为准。

## 0. 省流版

- 看了 **20+ 个**项目，分四类：**同源分叉**（同一份代码的不同维护线）、**独立重写 / 配套面板**、账号池网关、通用 AI 网关。
- 最该对标的是同源分叉 [misakano7545/workbuddy2api-panel](https://github.com/misakano7545/workbuddy2api-panel)：它还在维护，已经做完 `/v1/messages`，还多了生图/出视频 + MCP、思考档位策略、日志导出、劣化告警。
- 最值得抄的 6 件事：**多密钥分发**、**Anthropic `/v1/messages`**、**失败分类与重试预算语义**、**指纹规则数据化**、**审计闭环**、**指标导出**。
- 明确不抄：多租户计费、Gemini 原生协议、语义缓存、插件市场、设备指纹伪造。
- 我们并不落后：WAF IP 级 fail-fast、DSH 指纹脱敏 + system→user 搬移、模型级冷却、带前缀缓存维度的用量账本，这几项比多数同类做得细。

---

## 1. 我们现在的坐标

| 能力 | 现状 | 代码位置 |
|---|---|---|
| 鉴权 | **单把全局 `api_key`**，空 = 不鉴权 | `cmd/server/config.go`（`Config.APIKey`）、`internal/httpauth/httpauth.go` |
| 协议 | `/v1/chat/completions`、`/v1/responses`、`/v1/search/messages`；Anthropic 模型对话 `/v1/messages` 仍未实现，搜索端点不等于对话端点 | `internal/server/handler.go`（`routes`）、`internal/server/messages.go` |
| Anthropic 对话 | **没有**（`/v1/messages` 保留给未来的模型对话转发；现有 `messages.go` 只实现 `POST /v1/search/messages` 搜索壳） | `internal/server/handler.go`（路由注释）、`internal/server/messages.go` |
| 多账号池 | 加权随机 + 成本分层 + 冷却 / 熔断 / 在途租约 / 会话粘性 | `internal/pool/*` |
| 失败处置 | 429 分型、模型级冷却、WAF IP 级 fail-fast、通道拒绝 / 内容拦截分流 | `internal/pool/cooldown.go`、`internal/server/degrade.go`、`internal/server/wafip.go` |
| 用量账本 | 小时桶 / 日桶，含 latency、TPS、credit、前缀缓存命中 | `internal/usage/usage.go` |
| 请求日志 | 逐请求 JSONL，含 TTFB、attempts、token、credit、IP/UA | `internal/reqlog/reqlog.go` |
| 面板 | 7 个视图，内嵌单文件、免构建、热改配置 | `internal/panel/*` |
| 缺口 | 无密钥分发 / 配额、无审计、无 `/metrics`、无模型别名、无出口代理、无测试台 | — |

---

## 2. 项目地图

### 2.1 同源分叉（最该对标：同一份代码，不同人维护）

| 项目 | 状态 | 相对我们的增量 |
|---|---|---|
| [misakano7545/workbuddy2api-panel](https://github.com/misakano7545/workbuddy2api-panel)（约 1k★） | **仍在维护**；与上游同步止于 `ea8b1e5` 后独立演进 | 原生 `anthropic_messages`（`/v1/messages`，也接受 `/messages`）；三站点登录（CN / 官方国际站 / 旧桌面端）；思考档位策略可配（客户端优先 / 网关优先 + 作用域 + 按模型覆盖 + 尊重 `off`）；生图 / 改图 / 出视频 + `POST /mcp`；运行日志按天落盘、可导出 ZIP、重启回填；上游劣化滑窗告警（5 分钟窗 TTFB≥8s 占比≥30% 或 5xx≥10% 打聚合 WARN） |
| [HanawaBanana/workbuddy2api](https://github.com/HanawaBanana/workbuddy2api) | 上游删库后的延续仓 | 基线能力；主要价值是「血统可追溯」 |
| [ckldy/workbuddy2api](https://github.com/ckldy/workbuddy2api) | 小仓 | 参考价值有限 |

> 这条线才是真正的「同类」：同样的协议逆向、同样的账号池问题、同样的上游收紧。misakano 已经把 `/v1/messages` 做完，可以直接对照实现，少踩协议细节的坑；媒体生成 + MCP 是它目前拉开差距的地方。

### 2.2 独立实现 / 配套面板（腾讯 CodeBuddy / WorkBuddy 反代）

| 项目 | 定位 / 热度 | 亮点 | 可借鉴 |
|---|---|---|---|
| [ithtelab/workbuddy-manager](https://github.com/ithtelab/workbuddy-manager) | 独立 Web 控制台，约 2.6k★ | 多密钥分发（配额 / 有效期 / IP·模型白名单）、模型别名、全量审计、`/v1/messages` + `count_tokens`、`/v1/usage` 与 new-api 余额协议、只读 API Token、PG 异地备份、带 sha256/签名的自更新、watchdog、Windows 安装器 | **多密钥模型**、**审计**、**Anthropic 协议**、**余额协议兼容**、**只读令牌** |
| [ardeyouxipianyi/workbuddy2api-hub](https://github.com/ardeyouxipianyi/workbuddy2api-hub) | 多账号网关中枢，约 3.9k★ | 每账号独立出口代理槽、设备指纹隔离、多 API Key（模型 / 账号白名单 + 额度护栏）、便携版 / Docker / NAS / OpenWrt | **每账号出口**、**按 Key 限定账号范围**、**多端分发形态** |
| [CangShui/workbuddy-gateway](https://github.com/CangShui/workbuddy-gateway) | 纯 Go 单二进制 + 可选 Web UI，约 430★ | `identity.replacements` 15 条默认规则、**热加载**；`identity.responseRestore` 实验开关；`upstream.requestProfile` 统一出站；价格探测显式开关；9 平台构建 | **把指纹规则从代码搬到配置**、**出站 profile**、**危险动作 opt-in** |
| [linbeize/workbuddy2api-gui](https://github.com/linbeize/workbuddy2api-gui) | 第三方 Web 控制台（已停维护） | 仪表盘 15s 轮询、聊天测试台（流式 / reasoning / TTFB）、配置表单 + JSON 双模式且保存前备份、高危操作解锁 | **测试台**、**配置双模式 + 备份**、**高危确认** |
| [DistPub/workbuddy2api](https://github.com/DistPub/workbuddy2api) | 本机登录态转 API + 共享平台 | 独立 API Key + 按 Key 配额 + 用量记账；**对 Codex CLI 长上下文做压缩投影**；`--no-compact` / `--desensitize` 开关 | **压缩投影开关化**、**按 Key 记账** |
| [hawklithm/workbuddy2api](https://github.com/hawklithm/workbuddy2api) | 轻量 Python 代理 | 三协议（OpenAI / Anthropic / Responses）；**脱敏覆盖账号、密码、密钥、品牌词、路径**；消息压缩；DSML 工具调用；**按「账号 × 模型」记 429 冷却** + 同区故障转移；60s 超时保护 | **脱敏规则扩面**、**账号 × 模型粒度冷却** |
| [HanHan666666/codebuddy2openai](https://github.com/HanHan666666/codebuddy2openai) | 极简单文件中转 | 直连上游原生 tools，不做文本解析；读桌面端登录态；默认只监听 127.0.0.1 | **默认安全监听**、**能直连就别解析文本** |
| [baiyea/workbuddy2api-ui](https://github.com/baiyea/workbuddy2api-ui) | Docker Compose 一体化 UI | 自动签到、账号轮换、国际版 | 部署体验（一键 compose） |
| [cv-superding/WorkBuddy-Switch2api](https://github.com/cv-superding/WorkBuddy-Switch2api) | Tauri 桌面 + 本地反代 | 托盘常驻、账号切换、签到监控 | 桌面形态（非目标） |
| [Senguk520/CodeRelay-tools](https://github.com/Senguk520/CodeRelay-tools) | 账号池管理工具 | 模型同步与本地缓存、配额监控、托盘通知 | **模型目录缓存**、**通知** |

### 2.3 账号池网关（不同厂商，同一个问题）

| 项目 | 亮点 | 可借鉴 |
|---|---|---|
| [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) | 多 CLI 账号轮询；管理 API（配置热写、auth 文件上传下载、`usage-queue` 逐请求记录含 tokens 细分 / 延迟 / 失败 / request_id）；凭据级 `proxy-url`、`excluded-models`、模型别名；管理密钥 bcrypt 落盘；**连续 5 次认证失败封 30 分钟**；官方把用量统计剥离给外部面板 | **管理 API 契约**、**usage 队列字段设计**、**登录防爆破**、**核心保持精简** |
| [tbphp/gpt-load](https://github.com/tbphp/gpt-load) | 多渠道路由；凭据加密（`encryption.key`）；SQLite/MySQL/PG；权重、重试、冷却、黑名单、**会话亲和**；健康、日志、用量与成本估算；37+ 渠道 | **凭据加密落盘**、**成本估算**、**会话亲和** |
| [justlovemaki/AIClient-2-API](https://github.com/justlovemaki/AIClient2API) | 策略 / 适配器模式；账号池 + 健康检查 + 故障转移 + 配置降级；多协议互转；图片生成；50+ 管理端点 | **适配器分层**、**降级配置** |

### 2.4 通用 AI 网关（工程范式）

| 项目 | 亮点 | 可借鉴 |
|---|---|---|
| [songquanpeng/one-api](https://github.com/songquanpeng/one-api) / [QuantumNous/new-api](https://github.com/QuantumNous/new-api) | 令牌管理（过期 / 额度 / IP 范围 / 模型范围）、渠道分组与倍率、模型重定向、失败重试、渠道测试、用户 / 权限 / OAuth / 2FA、用量与成本账本、审计与日志 | **令牌四要素**、**模型重定向**、**渠道测试** |
| [maximhq/bifrost](https://github.com/maximhq/bifrost) | **重试 / 回退分类学**：per-key 永久失败直接换钥且不占重试预算；429 换钥但仍退避；5xx 同钥指数退避 + jitter；request failure 直接失败；timeout / TTFT 超时触发 fallback；取消记 499 不重试；全部凭据耗尽的语义化 `502 upstream_credentials_exhausted`；按 Key 配权重与可用模型；Prometheus / OTel | **失败分类矩阵**、**重试预算语义**、**语义化错误体**、**指标** |
| [BerriAI/litellm](https://docs.litellm.ai/docs/routing) | 路由策略（simple-shuffle / least-busy / usage-based / latency-based）、`num_retries`、`cooldown_time` + `allowed_fails`、fallback 链、预算与虚拟 Key、花费日志 | **策略可切换**、**冷却与失败阈值解耦** |
| [Portkey Gateway](https://github.com/Portkey-AI/gateway) | 配置即代码（JSON 描述路由 / 重试 / 超时 / 负载均衡 / 护栏）、guardrails、缓存 | **配置化路由**（长期） |

---

## 3. 可借鉴清单（按优先级）

### P0-1　多密钥分发：从「一把总钥」到「按 Key 管人管模型」

**问题**：现在只有一个 `api_key`，任何拿到它的人都能用全部模型、全部账号，且用量无法归因到「谁」。

**别人怎么做**：workbuddy-manager / workbuddy2api-hub / one-api 都是同一套四要素——**额度、有效期、IP 白名单、模型白名单**，外加「创建时展示一次、只存哈希」。

**我们怎么落地**（贴合现有风格，不引数据库）：

- 存储：`data/keys.json`，原子替换 + 防抖落盘（照抄 `pool/persist.go` 的写盘模式）。
- 结构：`id / name / hash(sha256) / prefix(前 8 位用于展示) / enabled / expires_at / quota_credits / quota_tokens / model_allow / ip_allow(CIDR) / created_at / last_used_at / usage{req,tokens,credits}`。
- 鉴权：`internal/httpauth` 增加 `Keyring.Verify`，先比对静态 `api_key`（兼容旧部署），再查 keys；仍是常量时间比较；命中后把 `key_id` 放进 context。
- 用量：`reqlog.Event` 增加 `key_id`；面板「运行日志」加 Key 列与筛选；`usage` 桶维度暂不扩（避免历史数据迁移），按 Key 的汇总直接读 keys.json 的累计计数器。
- 对外：`GET /v1/usage`（Key 自查：已用 / 剩余 / 有效期 / 模型范围）；`GET /api/usage/token/` 返回 new-api 余额协议形态，方便下游客户端显示余额。
- 面板：新增「密钥」视图，创建 / 停用 / 删除 / 复制一次 / 看用量；删除与停用写审计。
- 兼容：`api_key` 继续有效，文档注明「多密钥是叠加层，不是替代」。

**工作量**：M（2-3 天，含测试）。**风险**：低。**测试**：鉴权常量时间、配额边界、IP/模型白名单、并发写入。

---

### P0-2　Anthropic `POST /v1/messages`（Claude Code 直连）

**问题**：代码里只留了 `/v1/messages` 的路径占位，尚未实现 Anthropic 模型对话。Claude Code、部分 IDE 只认 Anthropic 协议，现在只能绕；`/v1/search/messages` 只服务搜索 provider，不能当对话端点用。

**别人怎么做**：workbuddy-manager、hawklithm、gpt-load、new-api 都把 `/v1/messages` 当标配；workbuddy-manager 还补了 `count_tokens`。**同源分叉 misakano 已经实现并跑通**（`anthropic_messages`，同时接受 `/messages`）——说明协议可行、坑位已知，对标它抄比从零摸更稳。

**我们怎么落地**：

- 入站改写：`system`（string / block 数组）→ OpenAI system；`content` block（text / image / tool_use / tool_result）→ OpenAI 消息；`tools` + `tool_choice` → OpenAI tools；`max_tokens` / `temperature` / `top_p` / `stop_sequences` 直映。
- 出站垫片：复用 `internal/responses` 的「事件翻译」写法，产出 `message_start` / `content_block_start` / `content_block_delta`（`text_delta`、`input_json_delta`、`thinking_delta`）/ `content_block_stop` / `message_delta`（`stop_reason`、`usage`）/ `message_stop`；非流式产出 `content` + `usage`。
- 复用管线：照样走选号、冷却、提示词脱敏、system→user 搬移；**不要**复制一份 chat 实现（照 `responses.go` 的「入站改写 + 出站翻译」模式）。
- `POST /v1/messages/count_tokens`：按字符数粗估（明确标注 estimate），满足客户端预检。
- 注意：`/v1/search/messages` 是搜索端点，不要混。

**工作量**：M（2-3 天，流式事件最费时）。**风险**：中（协议细节多）。**测试**：文本 / 图片 / 工具调用 / 流式断连 / 错误映射。

---

### P0-3　失败分类与重试预算：把「换号」和「重试」分开算

**问题**：我们的 429 分型已经不错，但「一次请求最多轮转几次」「换号算不算重试」「超时该不该 fallback」「客户端断开要不要记失败」这些语义还可以更明确、更可解释。

**别人怎么做**（bifrost 的写法最值得抄）：

| 失败类型 | 处理 | 是否占重试预算 |
|---|---|---|
| per-key 永久失败（401/403、额度耗尽、模型无权、下线） | 标记该 Key/账号本次请求内死亡，**立即换**，不退避 | 不占 |
| 429 限流 | 换号，但**仍退避**（账号级配额可能共享窗口） | 占（按配置） |
| 5xx / DNS / 连接拒绝 | **同号**重试，指数退避 + jitter | 占 |
| 请求本身失败（400 参数错） | 直接返回，不换号 | 不占 |
| 超时 / TTFT 超时 | 触发 fallback；请求总 deadline 到了就终止 | 占 |
| 客户端取消 | 记 499，不重试 | 不占 |

另外两条很实用：
- 全部凭据都因「账户级原因」失败时，返回 `502 upstream_credentials_exhausted`，而不是把上游 4xx 原样抛给客户端——排查时一眼能分清「我的 Key 没问题，是池子空了」。
- 每次请求在日志里落一个 `fail_reason` 枚举，面板可按原因聚合。

**我们怎么落地**：`internal/server/backoff.go` 已有退避骨架，补一个 `failureClass` 判定 + `retryBudget`；`reqlog.Event` 加 `FailReason`；`/status` 与面板日志透出。**工作量**：S-M（1-2 天）。**风险**：中（会改变线上重试行为，需灰度开关）。

---

### P0-4　指纹规则数据化：上游一收紧，改配置而不是改代码

**问题**：`internal/upstream/sanitize.go` 里的 DSH / Claude Code / Codex / 审查插件身份句是硬编码的。上游 11-128 的判据还在变（linbeize 的停维护公告、workbuddy2api-hub 的 10-23 收紧警告都说明了这点），每次都要发版。

**别人怎么做**：CangShui 的 `identity.replacements`——15 条默认规则、支持通配（单行、动态段 ≤256 字符）、热加载、`enabled` 开关、旧配置自动补默认但**不覆盖用户自定义**。

**我们怎么落地**：

- `config.json` 增 `identity.replacements[]`：`{match, replace, regex, roles:[system,developer,assistant], enabled}`；启动时把内置默认规则合并进去（缺的补，已有的不动）。
- `sanitize.go` 改为「内置规则 + 配置规则」两段式；配置段可热加载（走 `livecfg`）。
- 面板配置页加只读预览 + JSON 编辑（高危确认）。
- 加一个「命中统计」：每类规则命中次数写进日志/面板，方便判断哪条规则还有效、上游是不是换了新指纹。

**工作量**：M（2 天）。**风险**：中（正则误伤 user 内容——必须保持「只洗 system/developer/assistant」的边界，并保留现有测试）。

---

### P1-5　模型别名 + 模型目录元数据

- **别名**：`models.aliases`（`alias → 规范模型`，支持 `cn:` / `global:` 前缀），`resolveModel` 先查别名；`/v1/models` 同时列出别名与真名。解决「客户端内置模型名对不上」的迁移痛点（new-api 的「模型重定向」、workbuddy-manager 的「模型别名映射」都在做）。
- **元数据**：`/v1/models` 每项补 `context_length` / `max_output_tokens` / `capabilities`（tools / vision / thinking）/ `pricing_multiplier`。多数客户端会忽略多余字段，但 Cline、Cherry Studio 这类会用到上下文长度。数据我们面板里已有，只是没透出。
- **工作量**：S（半天到 1 天）。**风险**：低。

### P1-6　审计日志 + 鉴权防爆破 + 只读令牌

- **审计**：`data/audit.jsonl`，记录「谁（面板 Key / IP）、何时、做了什么（改配置 / 删账号 / 停用 / 建密钥 / 触发批量任务）、结果」。面板加「审计」筛选。这是 workbuddy-manager 明确做、我们完全空白的运营能力。
- **防爆破**：照 CLIProxyAPI 的做法——同一 IP 连续 N 次 Bearer 失败，封 M 分钟（内存状态即可，重启清零）。我们已有常量时间比较，补这一层成本很低。
- **只读令牌**：给脚本 / 监控用，只能读 `/status`、`/panel/api/overview` 等只读接口，不能改配置。workbuddy-manager 叫「管理面 API Token」。
- **工作量**：S（1 天）。**风险**：低。

### P1-7　指标导出（`/metrics`）

- 不引第三方库，手写文本 exposition（Prometheus 格式约 100 行）：
  `gateway_requests_total{model,outcome}`、`gateway_tokens_total{kind}`、`gateway_accounts{state}`、`gateway_cooldown_seconds{model}`、`gateway_ttft_seconds` 直方图、`gateway_inflight`。
- 或者先做 JSON 版 `/panel/api/metrics`，面板画图用；等有 Grafana 需求再上文本格式。
- **工作量**：S-M。**风险**：低（只读，不影响主链路）。

### P1-8　凭据加密落盘

- 现状：`auths/*.json` 是明文 accessToken / refreshToken，README 已经提示「被植入后门会偷凭证」。gpt-load 的做法是 `encryption.key` + 加密凭据，备份时必须带上 key。
- 落地：`auths` 写入时 AES-GCM 加密（key 来自 `data/encryption.key`，首次启动生成，权限 0600，或环境变量 `WB2API_MASTER_KEY`）；读取时透明解密；提供 `--reencrypt` 迁移命令。
- **工作量**：M（2 天，涉及 login / auth / panel 导入多处）。**风险**：中（key 丢失 = 全部重新登录，文档必须写清楚）。

### P2-9　面板「测试台」

- 一个页面直接调 `/v1/chat/completions`：选模型、流式 / 非流式、system prompt、reasoning 折叠、TTFB / token 用量、会话 ID 开关（验证粘性）。
- 纯前端 + 现有接口，**不改网关**；对回归验证（尤其改提示词 / 脱敏 / 协议后）价值很高。
- **工作量**：S（1 天）。**风险**：低。

### P2-10　每账号出口代理（谨慎项）

- 同类里 workbuddy2api-hub 做成「每账号独立出口代理槽」，用于多地区账号与出口隔离。
- 我们能做的最小版本：账号级 `proxy` 字段（http/socks5），每账号一个 `http.Transport`；`/status` 展示「是否配代理」；连通性探测单独按钮（**不自动打**，避免无谓出站）。
- **合规提醒**：这条只适合「账号归属地 / 公司出口 / 家庭宽带」这类正当网络环境；拿它绕风控、轮换住宅 IP 属于上游明确反对的用法，建议文档里写死边界，甚至默认关闭。
- **工作量**：M。**风险**：中（合规 + 连接复用 / 泄漏）。

### P2-11　长上下文压缩投影（谨慎项）

- DistPub 和 hawklithm 都对 Codex CLI 这类长上下文请求做压缩：裁历史 tool 输出、丢旧 reasoning、保留最近 N 轮。
- 我们的 `tool_pairing.go` 已经处理配对，做压缩要特别小心「裁了 tool 结果但留下 tool_call」导致上游 400。
- 建议：`request.projection` 默认关闭，规则保守（只裁「非最近 K 轮」的 tool 结果到 N 字符；永远保留 system 与最后一条 user；不碰 tool_call/tool_result 配对），并加「压缩前后 token 估算」日志。
- **工作量**：M-L。**风险**：中高（容易把 agent 会话改坏）。

### P2-12　更新 / 备份 / 通知

- **自更新**：workbuddy-manager 做了 sha256 + `.sig` 校验的一键更新；我们至少可以在 `/panel/api/version` 里查最新 Release，并给一条「下载 + 校验 + 替换」脚本，不做进程内自替换（风险高）。
- **备份**：配置 + `auths` + `data/` 打 zip，面板一键导出 / 导入；PG / 异地备份对自用场景偏重，可不做。
- **通知**：账号全挂、余额低于阈值、模型目录漂移时推送到 webhook / 邮件 / QQ（workbuddy-manager 有 watchdog + QQ 机器人）。最小版本：`notify.webhook` + 事件白名单。
- **工作量**：S-M。**风险**：低。

### P2-13　上游漂移探测

- 定期比对「上游模型目录 / 倍率 / 可用档位」与本地快照，变化时记日志 + 面板提示 +（可选）通知。我们已有 `catalog_audit` / `model_catalog` 的基础，补一个 diff 视图即可。
- **工作量**：S。**风险**：低。

### P2-14　媒体生成 + MCP（可选大件）

- misakano 把上游媒体模型包成了 OpenAI 形态：`POST /v1/images/generations`（文生图）、`/v1/images/edits`（改图，支持 URL / data URL / multipart）、`/v1/videos*`（异步任务 + 查询 + 取成品）、以及 `POST /mcp`（JSON-RPC 2.0 / Streamable HTTP，给 Claude Code / Codex / Cursor 一行配置就能调出图出视频工具）。
- 价值：对个人使用是「多一种能力」；对 agent 客户端是实打实的工具面扩展。
- 成本：**L**（异步任务状态机、签名直链有效期、记账、模型目录标记、超时与取消）。
- 风险：中。上游媒体接口稳定性未知；「去水印」这类后处理有合规争议，**建议不做**。
- 建议：先做「只读探测 + 面板展示媒体模型 / 倍率」，确认接口可用再上生成接口。别一上来就全做。

---

## 4. 不建议借鉴

| 方向 | 为什么不做 |
|---|---|
| 多用户 / 充值 / 兑换码 / 用户组倍率（one-api 路线） | 与「个人自用」定位冲突，还会把项目拖进合规定性与运维负担 |
| Gemini 原生协议、图片 / 视频生成、Realtime、插件市场 | 上游不提供或非核心场景；gpt-load 那套「37 渠道」是平台型产品的解法，不是我们的 |
| 语义缓存 | coding agent 请求几乎不重复，收益低；缓存正文还带来隐私问题 |
| 完整 OIDC / passkey / 2FA | 单用户自用，长随机 Key + IP 白名单 + 防爆破足够；这些是运营型产品的需求 |
| 设备指纹伪造 / 住宅 IP 轮换绕风控 | 违反上游条款，和 README 里「仅限本人授权账号」的立场直接冲突 |
| 进程内自动更新（下载即替换自身） | 失败面太大；脚本化 + 校验更稳 |
| 完整 HA / 集群 / 多租户隔离 | 个人自用场景用不上，Redis 镜像 + 单实例足够 |

---

## 5. 建议的落地顺序

**批次 A：兼容性（最划算，先做）**
1. 多密钥 v1（存储 / 鉴权 / 配额 / 白名单 / `/v1/usage` / 面板密钥页 / 审计埋点）
2. `/v1/messages` + `count_tokens`（模型对话；不要与 `/v1/search/messages` 混用）
3. 模型别名 + `/v1/models` 元数据

**批次 B：韧性**
4. 失败分类 + 重试预算 + `fail_reason` 落日志
5. 指纹规则数据化（`identity.replacements`）
6. 审计日志 + 防爆破 + 只读令牌

**批次 C：运维体验**
7. `/metrics`（先 JSON，后文本）
8. 测试台
9. 凭据加密（如决定做）
10. 更新 / 备份 / 通知（按需）

**先不做**：出口代理、上下文压缩、PG 备份——这三件事收益不确定、风险最高，等前面的做完再评估。

---

## 5.5 持续对标机制（值得做成例行任务）

misakano 仓库里有个 **AutoBot CI** 的做法：每天定时 clone 一批同类实现，让 agent 读代码写差异报告，再按报告写 Go 实现并开 PR（它自己标注「开发中，产出需人工筛选」）。

我们可以做个轻量版，成本很低：

1. `scripts/competitive-diff.sh`：固定克隆 5-6 个仓（misakano、CangShui、workbuddy-manager、hawklithm、hub），只抓三样东西——**HTTP 路由表、config 字段、面板视图列表**。
2. 用 `grep` / `ast` 出结构化清单，和本地对比，输出 `data/competitive-diff.md`（本地，不进库）。
3. 人工判断「哪些是真增量」，再决定做不做；不要自动开 PR。

好处：上游协议一变，同类里总有人先修；靠这个能比等 issue 早半步发现「别人已经解决了什么」。

---

## 6. 代码落点速查

| 改动 | 主要文件 | 预估 |
|---|---|---|
| 多密钥 | `internal/httpauth/`（新增 keyring）、`internal/reqlog/reqlog.go`、`internal/server/handler.go`、`internal/panel/panel.go` + 前端、`cmd/server/config.go` | M |
| `/v1/messages` | 新增 `internal/server/anthropic.go`（入站改写 + 出站事件），复用 `internal/responses/` 写法；与搜索端点 `/v1/search/messages` 分文件、分路由 | M |
| 失败分类 | `internal/server/backoff.go`、`internal/server/handler.go`、`internal/pool/cooldown.go` | S-M |
| 指纹规则 | `internal/upstream/sanitize.go`、`internal/livecfg/livecfg.go`、`cmd/server/config.go` | M |
| 别名 / 元数据 | `internal/server/resolve_model.go`、`models_fetch_log` 相关、`internal/modelid/` | S |
| 审计 | 新增 `internal/audit/`、面板各写操作入口 | S |
| 指标 | `internal/server/handler.go`（新路由）、`internal/usage/`、`internal/pool/` | S-M |
| 测试台 | `internal/panel/index.html` / `app.js`（纯前端） | S |

---

## 6.5 2026-11 补研：refresh token 真吊销的处置（已落地）

> 专题文档见 [refresh-token-lifecycle.md](refresh-token-lifecycle.md)。这一轮只查「会话/凭证被上游判死」这一条线，结论与采纳项：

- 同类**没有一家**能在 RT 真吊销后自动恢复：全部收敛到「三态求证 + 连续计数 + 面板重登」。
- 最值得抄的三件（均已采纳）：misakano 的 `VerifySessionByRefresh` 三态 + AT 仍有效豁免；
  manager 的「重登就地改写、保留 device_token、封号不进重登队列」；CangShui 的「覆盖/删除前只读校验」。
- 明确不抄：marker 命中即禁用（DistPub）、网络错误当吊销（WorkBuddy-Switch2api 反例）。
- 判据速记：单次 401/12153 不禁用；网络/5xx/429 不算；11140 与 12153 分型；RT 死 ≠ 账号死。

---

## 7. 参考链接
- workbuddy-manager：https://github.com/ithtelab/workbuddy-manager
- workbuddy2api-hub：https://github.com/ardeyouxipianyi/workbuddy2api-hub
- workbuddy-gateway：https://github.com/CangShui/workbuddy-gateway
- workbuddy2api-gui：https://github.com/linbeize/workbuddy2api-gui
- DistPub/workbuddy2api：https://github.com/DistPub/workbuddy2api
- hawklithm/workbuddy2api：https://github.com/hawklithm/workbuddy2api
- CLIProxyAPI：https://github.com/router-for-me/CLIProxyAPI ｜ 管理 API：https://help.router-for.me/cn/management/api
- gpt-load：https://github.com/tbphp/gpt-load
- AIClient2API：https://github.com/justlovemaki/AIClient-2-API
- one-api：https://github.com/songquanpeng/one-api ｜ new-api：https://github.com/QuantumNous/new-api
- bifrost：https://github.com/maximhq/bifrost ｜ 重试与回退：https://docs.getbifrost.ai/features/retries-and-fallbacks
- LiteLLM Router：https://docs.litellm.ai/docs/routing
- Portkey Gateway：https://github.com/Portkey-AI/gateway
