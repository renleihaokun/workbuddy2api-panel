# 出站身份：对齐真机抓包

> **文档性质**：当前实现说明 / 对照记录。若与根 README 或代码冲突，以根 README 与代码为准；修改相关实现时请同步本页。

> 2026-11-25。本文记录「网关出站请求长什么样」的现状、依据与开关。
> 口径来自同类项目对官方分发包的解包与真机抓包，**不是我们自己的抓包**——见文末「已知局限」。

## 0. 省流版

- 官方客户端**从不发** `X-Machine-ID` / `X-Session-ID`，设备标识在 `/v2/report` 的 **body** 里。我们以前发这两个头，是自造指纹，已删。
- UA 分**三族**（chat / RPC / billing），来自官方三条不同代码路径，版本按 realm 各取各的。
- chat 要带 `x-stainless-*`、`X-Agent-*`、`X-Private-Data`、`b3` / `traceparent` / `baggage`——这批「真机有、我们以前没有」的头才是「缺少出站指纹」的实指。
- 一键回退：`upstream.identity_mode = "legacy"`，热生效，不用回滚二进制。

## 1. 为什么改

改造前的问题不是「缺指纹」这么笼统，而是两头都错：

**多发的（自造）**

| 头 | 问题 |
|---|---|
| `X-Machine-ID` / `X-Session-ID` | 官方 chat 请求根本没有这两个头。app.asar（main/ + renderer/）与内置 cli/dist 全量字符串里 `X-Machine-ID` 命中 0 次；真机 40 个出站头逐一比对也没有。我们把按 uid 派生的 36 hex 塞进去，是纯自造指纹。 |
| `Origin` / `Referer` | RPC 层是 node fetch，没有浏览器同源语义，一律不带。我们全覆盖了。 |
| `Accept-Language` | 桌面端不发。「global → en-US」是按账号域臆测，实测不成立。 |
| `X-CodeBuddy-Request` / `X-Requested-With` | **只有** `/v2/chat/completions` 带。我们放在 `CommonHeaders` 里全出站覆盖，30+ 条业务路径都多一个头。 |
| `X-No-*` 哨兵头 | 那是 CodeBuddy CLI 的约定，WorkBuddy 桌面端个人号不发。 |

**少发的（真机有）**：`X-Private-Data`、8 个 `x-stainless-*`、`X-Agent-Intent/Type/Runtime`、`X-B3-ParentSpanId`、`b3`、`traceparent`、`baggage`、`X-Auth-Method`/`X-Id-Source`/`X-Userinfo`、`X-Client-Version/Platform/Product`。

**形态错的**：`X-Conversation-ID` 该是带连字符 UUIDv7（我们发 32 hex）；`X-Trace-ID` 不该回落成轮级 request id（真机两族恒不相等）；`X-Root-Request-ID` 不该等于轮级；`X-B3-SpanId` 不该复用 messageID 前缀（会带出时间前缀）。

**UA 全面过期且形态错**：CN 该是 `5.11.3 / CLI 2.176.0`；global 第二段该是**小写 `workbuddy-ai`**（不是 `WorkBuddy AI`）；且 chat 与 RPC 是两个家族（首段一个驼峰、一个 slug）；billing 是 `axios/1.16.1`；`/v3/config` 是两段而不是 `CodeBuddyIDE/4.12.0`。

## 2. 现在的形态

### 2.1 三族 UA

| 路径 | 形态 | 出处 |
|---|---|---|
| chat | `WorkBuddy/<v> <brand>/<v> CLI/<cliV>`，首段**恒驼峰** | OpenAI Node SDK `buildUserAgent` |
| RPC/REST | `<brand>/<v> <brand>/<v> CLI/<cliV>`，首段即品牌 slug | 客户端自建 fetch 层 |
| billing | `axios/1.16.1` | billing 域走 axios 实例 |
| `/v3/config` | `WorkBuddy/<v> CLI/<cliV>` | 目录探测专用 |
| web 层 | 浏览器串 | 渲染器（Chromium）发出 |

`brand` 按 realm：CN `WorkBuddy`、global `workbuddy-ai`（Electron asciiSlug 把 `WorkBuddy AI` 转成小写连字符）。

版本默认：CN `5.11.3` / CLI `2.176.0`；global `5.9.4` / CLI `2.165.0`。两个发行档不同，不能互相套用。

### 2.2 各域头族

**chat**（唯一走 SDK 形态的路径）：`Accept: application/json`、`X-Requested-With`、`X-CodeBuddy-Request: 1`、`X-Agent-Intent: craft`、`X-Agent-Type: main|auxiliary`、`X-Agent-Runtime: local-agent`、`X-Agent-Purpose: conversation`、8 个 `x-stainless-*`、`X-Private-Data`、`X-Product: SaaS`、`X-IDE-*`、`X-Domain`（裸主机名）、完整会话头族。

**RPC/REST**：`Accept: application/json, text/plain, */*`、`X-Product: SaaS`、RPC 族 UA、`X-Client-*`（仅 CN）。**不带** Origin/Referer/Accept-Language/X-Requested-With/X-CodeBuddy-Request。

**billing**：axios UA、`X-Request-ID`（随机 32 hex）、`X-Domain`、`X-Tenant-Id`（企业号）、`X-Client-*`。

**小程序**：先套 billing 头族，再 `dropClientIdentityHeaders` 撤掉桌面端的 `X-Client-*`，换成 mp 口径（否则会发出 `5.11.3 / miniprogram / WorkBuddy` 这种谁都不像的组合）。

### 2.3 会话头族

| 头 | 形态 | 语义 |
|---|---|---|
| `X-Conversation-ID` | 带连字符 UUIDv7 | 会话级，多轮稳定 |
| `X-Conversation-Request-ID` | 32 hex | 轮级聚合主键（一次 user send 内复用） |
| `X-Conversation-Message-ID` = `X-Request-ID` | 32 hex | 消息级，每条独立 |
| `X-Root-Request-ID` | 32 hex | 会话根（**不是**轮级值的复制） |
| `X-Trace-ID` | 32 hex | 独立链路 ID，**不回落** request id |
| `X-B3-SpanId` / `X-B3-ParentSpanId` | 16 hex | 独立随机（不复用 messageID 前缀） |
| `b3` / `traceparent` | — | 官方两版都双发 |
| `baggage` | 4 成员 | 两个 buddy 键**留空值**（`k=` 是 OTel 序列化形态，不是漏填） |

## 3. 每账号出口代理：不做

原计划参考同类项目做 per-account egress slot，**评估后放弃**：

- Azure NAT Gateway 挂多公网 IP：微软文档明确「多 IP 时网关可使用其中任意一个」，SNAT 端口池化分配，**无法按账号钉住出口**。
- VM 网卡挂多公网 IP：技术可行，但需开订阅特性 `Microsoft.Network/AllowMultipleIpConfigurationsPerNic`，且每个公网 IP 单独计费；为 24 个号开 24 个 IP 不值。
- 住宅/机房代理池：上游明确反对，本项目 `docs/competitive-research.md` 已列为「不建议」。

**接受事实**：整池共用一个出口 IP。因此改为**时间分化**（见下节）——出口分化不了，就在时间上错开。

## 4. 时间分化：`schedule.jitter_minutes`

排程只精确到小时，不抖动时 24 个账号会在同一整点做完全相同的动作（签到/上报/保活/夜猫子），**同一个出口 IP 上的同刻同动作是比 HTTP 头更直接的关联特征**。

配置：`schedule.jitter_minutes`（默认 45，显式 `0` = 关闭）。

- 每个账号按 `sha256("wb2a:jitter:<task>:<day>:<uid>)` 散列到该小时内的不同分钟；
- 跨重启稳定（重启不漂）、按自然日轮换、按任务区分；
- 窗口不取 60，留 15 分钟余量避免尾部账号撞进下一小时；
- **定时排程**按账号分钟级抖动分散；**面板手动全量触发**（签到 / 上报 / 保活 / 刷新 / 扫描 / 补偿）统一受 `pool.batch_concurrency`（默认 `1` 严格串行）与 `pool.batch_delay_*`（默认 `800~2000ms` 随机停顿）约束，且打开页面 / 切页仅读取内存缓存，不再自动扇出请求上游。

## 5. 开关与回退

```json
{
  "upstream": { "identity_mode": "capture" },
  "schedule": { "jitter_minutes": 45 }
}
```

| 值 | 行为 |
|---|---|
| `capture`（缺省/空） | 对齐真机：删自造头、UA 三族、补真机头族、UUIDv7 会话 ID |
| `legacy` | **逐字节还原改造前**：X-Machine-ID/X-Session-ID、Origin/Referer/Accept-Language 全覆盖、旧三段式 UA（5.5.4 / 2.137.1） |

`identity_mode` **热生效**（面板保存即切，不用重启）；`legacy` 下连旧版本号一起还原——否则「旧头族 + 新版本号」是个谁都没见过的组合。

单独核对两种模式的真实出站头：

```bash
go run ./cmd/hdrprobe capture   # 42 个头
go run ./cmd/hdrprobe legacy    # 29 个头（含自造头，属预期）
```

## 6. 已知局限

1. **没有自己的抓包**。以上口径全部来自同类项目对官方分发包的解包与真机抓包（`misakano7545/workbuddy2api-panel` 的 app.asar + cli/dist 逆向、`workbuddy2api-hub` 的 mitmproxy 记录）。我们无法本地自证，这是本方案的核心风险点——所以必须有 `identity_mode` 这条退路。
2. **版本号会过期**。`5.11.3 / 5.9.4` 是 2026-11 的分发包版本；上游发新版后需重新核对，改配置即可，不用改代码。
3. **`/v3/config` 的探测 UA 与身份分离**。当开启 `legacy_catalog_probe` 时，不同 UA 路与企业端点之间会顺序探测并加入微短停顿（历史 UA **只在探测里出现**，不进任何用户链路）；默认仅使用当前官方桌面 UA 与企业端点顺序合并。
4. **TLS 指纹未对齐**。Go 的 ClientHello 与 Electron/Node 不同。同类项目有 `tls_fingerprint: node`（uTLS），我们未做——header 做到满分仍可被 TLS 层区分。属独立议题。
5. **迁移会改变 `/v2/report` body 里的 machineId 形态**（36 hex → 带连字符 UUID）。该字段只影响行为事件的设备标识，不影响鉴权与计费。

## 7. 代码落点

| 内容 | 位置 |
|---|---|
| UA 三族 / 版本 / 归属头 / 身份三头 / 出站身份模式 | `internal/upstream/identity.go` |
| 头族拼装（Chat/Common/Billing/Refresh） | `internal/upstream/headers.go` |
| 会话 ID 生成（UUIDv7 / SpanID） | `internal/session/ids.go` |
| 时间分化 | `internal/scheduler/jitter.go` |
| 设备 ID（body 用） | `internal/upstream/identity.go` 的 `deviceID` |
| JWT 声明读取（token_source / id_source） | `internal/auth/auth.go` |
| 出站头实测工具 | `cmd/hdrprobe/main.go` |
