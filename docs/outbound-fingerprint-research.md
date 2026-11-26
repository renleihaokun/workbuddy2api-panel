# WorkBuddy / CodeBuddy 反向代理网关：出站设备指纹与 UA 伪装调研

调研时间：2026-11-25。方法：优先读仓库源码与 docs，其次 README。

> **状态说明（2026-11-26）**：本文保留当时的对比观察；已落地的最终出站形态、开关与代码落点以 [outbound-identity.md](./outbound-identity.md) 为准。文中提到的旧 UA/版本/头清单可能已被后续提交覆盖，不能直接当现行配置参考。

> **更正声明（2026-11-25 复核）**
>
> 本文第 7 节第 1 条称「本项目缺 `X-Conversation-*` 会话头族」——**该结论对当前代码不成立**。
> `internal/upstream/headers.go` 的 `injectConversationHeaders` 一直在 chat/search 路径上
> 发全套会话头族；缺少的是 billing 与 desktop 那几条路径，已在本次改造中一并收敛。
>
> 其余结论（官方不发 `X-Machine-ID`/`X-Session-ID`、UA 分族与版本、billing 用 axios、
> 不自造 `X-Device-Token` 等）经二次核对后**成立**，并已落地。落地细节与最终口径见
> [outbound-identity.md](./outbound-identity.md)。

## 0. 覆盖度先声明

| 仓库 | 状态 |
|---|---|
| `misakano7545/workbuddy2api-panel` | **已读源码**（调研时 checkout；同源 `linguo2625469/workbuddy2api-panel`） |
| `CangShui/workbuddy-gateway` | **已读源码**（`outboundcompat.go`、`config.example.json`）+ `docs/outbound-compatibility.md` |
| `ardeyouxipianyi/workbuddy2api-hub` | **已读** `wb_fingerprint.py`、`docs/TROUBLESHOOTING-11-128.md`、README、PR #381 描述 |
| `ithtelab/workbuddy-manager`、`HanHan666666/codebuddy2openai`、`hawklithm/workbuddy2api`、`DistPub/workbuddy2api` | **未验证**（本轮预算耗尽，未读一行代码）。以下任何结论都不来自它们。 |

---

## 1. 出站请求头

### 1.1 panel（Go）

`internal/upstream/headers.go` `CommonHeaders`（所有出站共用）：

```
Content-Type: application/json
Accept: application/json
X-Requested-With: XMLHttpRequest
Origin / Referer: https://www.codebuddy.cn  (global 账号 → https://www.workbuddy.ai)
User-Agent: <见 1.3>
X-CodeBuddy-Request: 1
Accept-Language: zh-CN  (global → en-US)
X-Machine-ID / X-Session-ID: <见第 2 节>
```

`ChatHeaders` 追加：`Authorization: Bearer`、`X-User-Id`、`X-Enterprise-Id` 或 `X-No-Enterprise-Id: 1`、`X-Domain`、以及归属头 `X-Agent-Purpose: conversation` + `X-IDE-Name/Type` + `X-IDE-Version` + `X-Product`（`injectAttribution`，`headers.go:341-352`）。

会话头族（`injectConversationHeaders`，`headers.go:269-307`）——**这是关键**：

```
X-Conversation-ID            会话级，多轮稳定
X-Conversation-Request-ID    轮级聚合主键，必发
X-Conversation-Message-ID    消息级 = X-Request-ID
X-Request-ID                 = 同一 32 位 hex
X-Root-Request-ID            = conversationRequestID
X-Trace-ID                   入站透传，空则回退
X-B3-TraceId / X-B3-SpanId / X-B3-Sampled: 1
```

`BillingHeaders`（`headers.go:386-411`）额外带 `X-Tenant-Id: <EnterpriseID>`（仅此处有）。`X-Client-Platform` 只用于小程序口径（`school.go:115` = `mp-weixin`，`tasks.go` = `miniprogram`），**不在 chat 路径**。

### 1.2 CangShui（Go）

`outboundcompat.go` `applyObservedDesktopHeaders`，仅对 `POST /v2/chat/completions` 且 `prof.Key == "cn"` 生效。先**删除**：

```
Origin, Referer, X-Client-ID, X-Client-Version, X-Refresh-Token,
X-No-Authorization, X-No-User-Id, X-No-Enterprise-Id, X-No-Department-Info
```

再**设置**（2026-10-21 国内桌面实抓）：

```
Accept: application/json
User-Agent: WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0
X-Requested-With: XMLHttpRequest
x-stainless-arch/os/lang: x64 / Windows / js
x-stainless-package-version: 6.25.0
x-stainless-runtime: node       x-stainless-runtime-version: v22.21.1
x-stainless-retry-count: 0
X-IDE-Type / X-IDE-Name: WorkBuddy
X-IDE-Version: 5.8.3
X-Private-Data: true
X-Codebuddy-Request: 1
X-Agent-Intent: craft
X-Agent-Purpose: conversation
X-Agent-Type: main
X-Product: SaaS
X-Request-ID = X-Conversation-Message-ID（去连字符）
```

并对模型 JSON 做 gzip（`encodeModelRequest`），`Content-Length` 对齐压缩后字节。

值得注意：CangShui **不发** `X-Machine-ID` / `X-Session-ID`，且 `X-Product` 是 `SaaS` 而非 `WorkBuddy`——两家在归属头上取值相反，但都能跑，说明这组头不是硬闸门。

### 1.3 UA 三段式与版本号

panel（`headers.go:54-67`）：

```go
platform := "WorkBuddy"
if a.IsGlobal() { platform = "WorkBuddy AI" }
return "WorkBuddy/" + c.clientVersion() + " " + platform + "/" + c.clientVersion() + " CLI/" + c.cliVersion()
```

默认 `client_version = "5.5.4"`、`cli_version = "2.137.1"`。注释写明：global 账号平台段送错（`WorkBuddy` 而非 `WorkBuddy AI`）可能触发上游 403 code **11140** "request illegal"。billing/签到类用**单段** UA `WorkBuddy/<ver>`（`billingUA`）。另有独立常量 `desktopUA = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"` 用于 `/v2/report`（`desktop.go:44`）。

CangShui 只有一个常量 `observedDesktopUA = "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0"`，中间段版本与 `WorkBuddy/` 段**同值**，且与 `X-IDE-Version: 5.8.3` 一致。

hub README 记录国际版实测到 5.8.4 / 5.9.4，国内版对齐 5.8.3 / CLI 2.161.0。

**关于你当前的 `<platform>` 段**：三家都确认第二段不是品牌随便填——CN 是 `WorkBuddy`，global 是 `WorkBuddy AI`。

---

## 2. 每账号设备身份怎么生成

两家都是**无状态哈希派生**，没有一家用「随机 UUID 持久化到账号文件」：

panel（`headers.go:131-134`）：

```go
func deriveAccountStableID(uid, purpose string) string {
    sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
    return hex.EncodeToString(sum[:18]) // 36 hex chars
}
```

用途盐 `machine` → `X-Machine-ID`，`session` → `X-Session-ID`。`desktop.go:52` 另有一套（无 `wb2a:` 前缀）`sha256(salt + ":" + uid)[:18]`，供上报 body 的 `machineId` / `sessionId` / `qimei36` 复用。uid 为空时不注入。

hub（`wb_fingerprint.py`）——**并且它明确说 panel 那种形态是错的**：

```python
def derive_id(uid, salt):
    seed = f"{salt}:{uid or 'anonymous'}"
    h = hashlib.md5(seed.encode()).hexdigest()
    h = h[:12] + "4" + h[13:16] + "a" + h[17:]   # 强制 UUIDv4 版本位/变体位
    return "%s-%s-%s-%s-%s" % (h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
```

模块 docstring 原文：

> 形态按 5.8.4 抓包对齐（2026-10-16）：官方客户端的 machineId 取自 OS 硬件 UUID、sessionId 取自「每进程一个 randomUUID」，两者都是**带连字符的 UUID**；请求 id 是 **32 位无连字符 hex**。此前这里输出的是 36 位无连字符 hex 和自造的「前缀-微秒」串，形态都对不上，**本身就是一处「网关特征」**。

以及：

> 官方客户端「一个进程 = 一台设备」，而网关是一个长驻进程同时冒充几十台设备。所以 machineId / sessionId 都必须**按账号派生**——照官方那样在进程维度取一次，等于让所有账号共用同一个值。

`generate_request_id(uid)` 用 `uuid.uuid4().hex`（每条随机、**不掺 uid**），注释说掺 uid 前缀反而是稳定网关特征。

**结论**：设备身份 = `hash(salt:uid)` 确定性派生，per-account 隔离；分两级——machine 级跨会话稳定，session 级本应每进程一个。

---

## 3. X-Device-Token 是否必需

**三家一致：非必需，且都不伪造。**

- panel（`device_token.go`）：三源优先 `auth 每号 → config 全局 → 文件兜底`，文件 5 分钟缓存、>1KB 忽略，取不到则**不注入该头**（注释：「优雅降级不注入，不影响主流程」）。来源说明写得很直白：

  > 容器内无桌面端 Turing Shield SDK，无法像 Python fork（xiaofan6ya/converter.py）那样现取 token。

- CangShui（`outboundcompat.go` 注释）：`不伪造 X-Device-Token、SDK/Node 运行时信息或其他无法取得的客户端凭证`；docs 里列为「以下字段不凭空制造」。

- hub（`TROUBLESHOOTING-11-128.md`）：国内版桌面身份**可能**带 `X-Device-Token`，国际版和 CLI 身份**不带**；`accounts/settings.json` 有 `device_token` / `device_token_file`；并明确「没有真实 token 时不要伪造一个随机值；网关自己也不会为 CLI 身份生成设备 token」，且标注这是「排查项，不是 11-128 的已证实根因」。

**关于 11128 / 「request illegal」**——真正的证据指向会话头，不是设备 token：

- panel `hint.go:59-61` 把 11128 归 `ErrChannelRefused`，hint 文案：`upstream rejected the request channel (it may not look like the official client); check user-agent/headers or an intercepting proxy`。
- panel `headers.go:277-280` 注释：国际端点收紧通道校验后，**缺 `X-Conversation-ID` 的请求会直接 400 / 11128（conversation id missing）**。
- hub PR #381 独立复现同一结论：VSC/CLI 身份一直带 `X-Conversation-*`，只有桌面身份一个都没带，「所以被拦的总是它」。
- 11-128 全文是 `Illegal API invocation from an unapproved channel`，hub 明确：「不是账号被封」，换号换 Key 无用。
- 11140 另有「账号级 request illegal」含义（panel `hint.go:143-148` 要求 code + 文案双条件才判账号故障）。

---

## 4. UA 是否每请求随机

**没有一家随机化，全部确定性。**

- panel `headers.go:54-60` 注释：`官方无任何 UA 随机化，故默认确定性`。
- CangShui 是单一 `const`，全进程固定。
- hub 的版本号从面板集中配置（「出站身份版本号可查看与修改」），一经设定恒定。

**版本耦合**：两家都是同一个 `client_version` 同时喂 UA 两段与 `X-IDE-Version`（CangShui 硬编码 5.8.3 三处一致；panel 用 `c.clientVersion()`）。注意 panel 内部有一处不一致——`desktop.go` 用 5.5.6 而 `headers.go` 默认 5.5.4，属自己埋的坑，写计划时用**单一版本源**。

---

## 5. 每账号出口代理槽

只有 hub 有。README「核心特性」：`每账号独立出口代理槽绑定、设备指纹隔离`；支持 HTTP/SOCKS5 + 账号密码认证，`账号身份请求与出站调用均自动走该代理`。`usage.jsonl` 失败行记 `proxy_slot` / `proxy_exit`。另有 `WB_TLS_FINGERPRINT=auto|node|python` 的 Go/uTLS ClientHello 桥。

panel 与 CangShui **都没有 per-account 代理**：panel 是 `transport.go:88` 的 `Proxy: http.ProxyFromEnvironment`（全局 env），CangShui 配置里也没有代理字段。

---

## 6. 配置 schema

**CangShui `upstream.requestProfile`**（`config.example.json`）：

```json
"upstream": { "requestProfile": "official" }
```

取值 `legacy`（默认/空/null）或 `official`，非法值拒绝。热加载，一次逻辑请求只取一次快照。打开后仅国内模型 `POST /v2/chat/completions` 切到上面 1.2 那套头 + gzip；国际站保持 legacy。

**CangShui `identity.replacements`** —— 注意：**这和设备指纹无关**，是 **prompt 文本里的产品名替换**：

```json
"identity": {
  "enabled": true,
  "target": "WorkBuddy",
  "responseRestore": false,
  "replacements": { "Claude Code": "WorkBuddy", "DeepSeek Harness": "WorkBuddy", "Grok": "WorkBuddy" }
}
```

扁平 `map[string]string`（原文 → 替换文本），15 条默认规则，只作用于 system/developer/assistant 文本，不碰 user/tool/工具参数。

**panel `upstream`**：

```json
"user_agent": "", "client_version": "", "cli_version": "", "client_name": "",
"device_token": "", "device_token_file": "", "passthrough_ip": false
```

`client_name` 空 = 默认 `WorkBuddy`（发 `X-IDE-*` 四头）；配 `"SaaS"` 还原旧行为（只发 `X-Product: SaaS`，不设 `X-IDE-*`）。

**hub**：`accounts/settings.json` 的 `device_token` / `device_token_file`；env `WB_IDENTITY_OS` / `WB_IDENTITY_ARCH` / `WB_IDENTITY_NODE_VERSION`（**仅 CLI 身份生效**，WB/VSC 身份发了也没用）；`WB_CN_CHAT_UPSTREAM`、`WB_SANITIZE_LOOSE_BILLING`。

---

## 7. 可直接照抄的技术（按对你收益排序）

你现在发：确定性 per-uid `X-Machine-ID`/`X-Session-ID`、三段式 UA、`X-IDE-*`、`X-CodeBuddy-Request: 1`、**无** `X-Device-Token`。

1. **补齐会话头族（最高优先）**。`X-Conversation-ID` / `X-Conversation-Request-ID` / `X-Conversation-Message-ID` / `X-Request-ID` 同时发，`X-Request-ID` 与 `X-Conversation-Message-ID` 取同一个 32 位 hex。这是 panel 与 hub **两条独立证据**共同指向的 11128 / `conversation id missing` 根因。你的清单里没有这组头——先补这个。
2. **把 `X-Machine-ID`/`X-Session-ID` 改成带连字符的 UUIDv4 形态**。现在的 36 位无连字符 hex 正是 hub 点名的「网关特征」。照抄 `wb_fingerprint.py` 的 `derive_id`：`md5(salt:uid)` → 强制 index12='4'、index16='a' → 8-4-4-4-12。改动极小，语义不变。
3. **`X-Request-ID` 改成每请求随机 32 hex**，别用 `前缀+纳秒`（panel `desktop.go:563` 就是这么写的，也被 hub 批评为自造串）。
4. **保持 UA 确定性，并让版本单一来源**。别随机化；`WorkBuddy/<v>` 两段、`CLI/<cliV>`、`X-IDE-Version` 全部取自同一个 `client_version`。CN 用 `WorkBuddy` 平台段，global 用 `WorkBuddy AI`（送错 → 11140）。
5. **`X-Device-Token` 继续不发**。三家都不伪造，hub 明说不是 11-128 根因。若要支持，做三源优先级（per-account → 全局 → 文件兜底，5 分钟缓存、>1KB 忽略），取不到就不注入，别卡主流程。
6. **补 CangShui 抓到但你可能缺的：`X-Agent-Intent: craft`、`X-Agent-Type: main`、`X-Private-Data: true`、`X-Agent-Purpose: conversation`**。注意 `X-Product` 两家取值相反（`SaaS` vs `WorkBuddy`），说明它不敏感，跟随 `client_name` 配置即可。
7. **清理 `X-No-*` 头**。CangShui 在 official 模式下显式删除 `X-No-Authorization` / `X-No-User-Id` / `X-No-Enterprise-Id` / `X-No-Department-Info`；panel 仍在发。
8. **`x-stainless-*` 只在自称 CLI 身份时发**（hub 明确 WB/VSC 身份不发）。别把 CLI 的 `x-stainless-os: Windows` 混进桌面身份。
9. **per-account 出口代理槽**（可选，成本最高）：hub 是唯一实现的，且要求「身份请求与出站调用都走代理」才谈得上指纹隔离。你现在的 `ProxyFromEnvironment` 会让整池共享一个出口 IP——这是比任何 header 都强的关联键。

**未能验证**：`ithtelab/workbuddy-manager`、`HanHan666666/codebuddy2openai`、`hawklithm/workbuddy2api`、`DistPub/workbuddy2api` 四仓库一行代码都没读；hub 的 `wb_identity.py` 现已 404（我只看到搜索快照，未读到原文），其中 WB/VSC/CLI 三身份的具体头差异按**未验证**处理；`X-Device-Token` 对成功率的实际影响，没有任何项目给出量化证据。
