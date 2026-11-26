# refresh token 的生命周期与「真吊销」处置

> **文档性质**：当前实现说明 / 对照记录。若与根 README 或代码冲突，以根 README 与代码为准；修改相关实现时请同步本页。

> 适用范围：WorkBuddy 反代（本仓）的账号凭证。写完这一版后，「一次 401 就把号踢出池」不再发生。
> 相关代码：`internal/upstream/verify.go`、`internal/pool/{state,transition,persist}.go`、
> `internal/server/handler.go`（`penalizeSessionDead`）、`internal/scheduler/scheduler.go`、
> `internal/panel/{login,panel,import}.go`、`internal/auth/auth.go`。

## 0. 一句话结论

上游的 refresh token 是**一次性轮换票**：每刷一次换一张新的，旧票立刻作废。真被判死（作废）之后
**没有任何代码能救回来**，唯一出路是人再扫码登录一次。所以网关能做、也只该做的三件事是：

1. **别误判**——一次 401 / 一次 12153 不构成「真吊销」；
2. **别二次伤害**——不要把还能用的号禁用，也不要用过期导出覆盖好凭证；
3. **让重登便宜**——面板一键重登、就地改写、保留设备令牌、成功后自动解禁。

## 1. 两个正交的概念（先分清，否则必误判）

| 概念 | 含义 | 判据 | 后果 |
|---|---|---|---|
| **RT 已死**（凭证层） | 上游作废了 refresh token，无法再续期 | refresh 端点被应用层明确拒绝（`12153` / `invalid_grant`） | 现在可能**仍然可用**（access token 没过期）；到期后只能重登 |
| **账号不可用**（账号层） | 上游拒绝了这个号的请求 | chat/搜索拿到 401 且经求证确认，或 AT 已过期 | 退出选号池；`session_dead` 需重登，`banned` 需申诉 |

把两者混为一谈，就会得到两种真实事故：

- 拿「RT 死」当「账号死」→ 连续三次保活把**完全健康**的号禁掉（同类项目实测：29 个号里 4 个
  refresh 恒回 12153、AT 却还有 26~333 天、chat 全 200）；
- 拿「一次 401」当「账号死」→ 上游边缘抖动一次就批量掉号（本仓 issue #215：十个号掉了七个，
  睡一觉又自己好了）。

## 2. 上游的三类失效形态

| 形态 | 现场长相 | 归类 | 处置 |
|---|---|---|---|
| 会话被注销 | `401 {"code":12153,"msg":"Offline user session not found"}` | `ErrSessionDead` | 401 先求证；连续 3 次确证才禁用 |
| refresh 票被作废 | `401 {"code":12153,"msg":"12153:refresh token is invalid"}` / `invalid_grant` | 同上（来自 refresh 端点） | AT 未过期 → 只标 `refresh_token_dead`；AT 已过期 → 计数 |
| 网关/边缘拦截 | `401` + APISIX/openresty **HTML 页**（无 `code`/`msg` 信封） | `ErrClient`（不罚号）→ 401 求证路径 | 求证：reference 成功=撤销；refresh 也被拦=无判词 |

补充两类容易误判的：

- **11140「账号存在异常行为」** = 账号级封禁，重登**无效**，只能官方申诉。判据必须同时有
  11140 + `request illegal`/`异常行为`（11140 也能表示模型级限流）。
- **429** = 限流，不是吊销。上游大面积 429 时按软冷却处理，别升级成禁用。

## 3. 三态求证（`upstream.VerifySession`）

401 到手后，拿 refresh 端点问一次，只有三种结论：

```
                     ┌─ refresh 200            → Alive       会话还活着，401 只是被拦
401 → VerifySession ─┼─ refresh 4xx（业务信封） → Dead        上游应用层明确拒绝 → 计数
                     └─ 其它                  → Inconclusive 网络/5xx/429/408/网关拦截/WAF
```

要点（每条都踩过）：

- **`Inconclusive` 绝不能当 `Dead`**：refresh 端点同样挂在 `/v2/*` 上，边缘拦截时它也回 HTML 页。
  把「refresh 也失败」一律判死，就是在同一层再误杀一次。
- **求证成功要立刻落盘**：上游已轮换 RT，不写回则重启回滚到旧票 —— 本来没死的号会被自己弄死。
  这一步由 `VerifySession` 内部 `SaveAtomic` 负责，失败只打 `ERR` 不改结论。
- **节流**：同一账号 `pool.session_verify_interval`（默认 60s）内只求证一次，窗口内复用结论
  （`Dead` 继续计数、`Alive` 继续不罚）。上游大面积 401 时不给刷新端点造风暴。
- **单飞复用**：求证与保活/请求路径共享同一条刷新单飞，不会双花 RT。

开关：`pool.verify_session_on_401=false` 完全回到旧行为（401 直接进连续计数）。这是唯一回滚闸，
热生效。

## 4. 处置矩阵

| 事件 | 动作 | 落库字段 | 面板表现 |
|---|---|---|---|
| chat 401，求证 Alive | 清连续计数 + 30s 短冷却 | — | 正常（短暂「限流冷却」） |
| chat 401，求证 Dead | `NoteSessionDead`（+1） | `session_dead_fails` | 正常 |
| chat 401，求证无判词 | 不计数 + 60s 短冷却 | — | 正常（短暂冷却） |
| 连续 3 次 Dead | 禁用 | `disabled=true`、`disabled_kind=session_dead` | 红标「待重登」+ 重登按钮 |
| 请求前刷新被判死、AT 仍有效 | 不禁用，继续用 AT 发请求 | `refresh_token_dead_at` | 黄标「RT 已失效 · AT 剩 N 天」 |
| 请求前刷新被判死、AT 已过期 | 计数（+1），换号 | `refresh_token_dead_at`、`session_dead_fails` | 正常 |
| 11140 封禁 | 硬禁用 | `disabled_kind=banned` | 「已禁用」（无重登按钮，提示申诉） |
| 面板人工禁用 | 禁用 | `disabled_kind=manual` | 「已禁用」+ 解冻 |
| 重登成功 | 清禁用/死因/RT 判死标记 | 全部清空 | 回到「可用」 |
| 刷新成功（手动/保活） | 清 RT 判死标记 | `refresh_token_dead_at` 清零 | 黄标消失 |

## 5. 状态字段（`/status`、`/panel/api/overview`）

| 字段 | 类型 | 含义 |
|---|---|---|
| `disabled` | bool | 已退出选号池 |
| `disabled_kind` | string | `session_dead` / `banned` / `manual` / 空（未分型） |
| `needs_relogin` | bool | `disabled && disabled_kind==session_dead`（派生，不落盘） |
| `refresh_token_dead` | bool | RT 被上游判死（账号可能仍可用） |
| `refresh_token_dead_at` | time | RT 判死时刻 |
| `access_token_expires_at` | time | 当前 AT 到期时刻（面板据此算「AT 剩 N 天」） |

`overview` 额外给 `needs_relogin` 计数（概况里单独一行提示）。

持久化（`state.json`）：新增 `disabled_kind`、`refresh_token_dead_at` 两个键，旧文件缺键 = 零值。
**升级迁移**：旧 state 里 `disabled + reason` 含 `12153`/`session dead` 的账号按 `session_dead`
一次性兜底分型，升级后立即可重登，不必等下一次 12153。

## 6. 运维手册

### 6.1 怎么判断是「真吊销」而不是抖动

1. 面板点该号「刷新令牌」（`POST /panel/api/accounts/{uid}/refresh`）；
2. 回包 `needs_relogin=true` 且 `access_usable=false` → 真吊销，只能重登；
3. 回包 `needs_relogin=false` → 网络/上游抖动，等几分钟再试；
4. 回包 `needs_relogin=true` 且 `access_usable=true` → RT 真死了但号还能用，**趁早重登**。

命令行等价手段：直接打 `POST {chat_base}/v2/plugin/auth/token/refresh`（`X-Refresh-Token: <RT>`），
看是不是 4xx 业务信封。注意这一步会轮换 RT，**必须**把返回的新 token 写回，否则等于手动把号弄死。

### 6.2 怎么重登

面板账号行点「重登」→ 浏览器打开授权页扫码 → 完成。后端做的事：

- realm 跟账号走（不跟界面开关），避免把国际版号写成 `cn`；
- 扫回来的 uid 必须等于目标 uid，否则**一个字节都不动**并报错；
- 就地改写该 uid 已存在的那份 auth 文件（不新建第二份），只覆盖 `auth`/`account` 两段；
- 保留 `device_token` / `proxy` / `weight` 等手写键（并同步回内存，防止后续 refresh 再写没）；
- 成功后清 `disabled` + `disabled_kind` + `refresh_token_dead_at`，账号回到池中。

### 6.3 为什么不做自动重登

上游只提供扫码 / 手机号 / 浏览器授权，没有可无人值守的兑换路径（同类项目无一实现）。
存密码去模拟登录属于绕过验证，本项目明令不做。

### 6.4 什么情况下别重登

- `disabled_kind=banned`（11140 异常行为）：重登不解除限制，只会白扫一次码；
- 上游大面积 401 期间：先看「刷新令牌」的回包判断是不是边缘拦截，别批量重登。

## 7. 防误杀清单

1. 单次 401 / 12153 不禁用（连续 3 次确证）。
2. 401 先求证：`Alive` 撤销、`Inconclusive` 不计数。
3. 网络错误 / 5xx / 429 / 408 一律不算「吊销证据」。
4. RT 死但 AT 有效 → 不禁用、不计数、继续服务。
5. 刷新成功 / 任意成功 / 手工复活 → 清连续计数。
6. 11140 封禁与 12153 会话失效分型，处置互不串门。
7. 重登/导入就地改写，绝不冲掉 `device_token`。
8. 覆盖导入默认关闭（`overwrite=1` 才覆盖），删除健康号需 `force=1`。

## 8. 同类项目对照（2026-11 调研）

| 项目 | 做法 | 采纳情况 |
|---|---|---|
| [misakano7545/workbuddy2api-panel](https://github.com/misakano7545/workbuddy2api-panel) | `VerifySessionByRefresh` 三态（alive/dead/inconclusive）+ `AccessUsableWithoutRefresh` 豁免 + 60s 求证宽限 | **全采纳**（本仓 `verify.go` 同口径） |
| [ithtelab/workbuddy-manager](https://github.com/ithtelab/workbuddy-manager) | refresh 429 保护性暂停（Retry-After，3 次升级人工）、重登队列、封号不进队列、就地改写保留 device_token | 部分采纳（就地面板重登 + 分型；429 暂停未做，列入后续） |
| [CangShui/workbuddy-gateway](https://github.com/CangShui/workbuddy-gateway) | 覆盖/删除前只读校验；保活串行 1s 间隔；RT 到期预警 | 采纳「删除/覆盖前护栏」；到期预警列入后续 |
| [hawklithm/workbuddy2api](https://github.com/hawklithm/workbuddy2api) | 登录先写临时会话文件，成功才入库；按账号重登 | 精神采纳（uid 校验 + 失败不动文件） |
| [DistPub/workbuddy2api](https://github.com/DistPub/workbuddy2api) | marker 命中即禁用（无三振） | 不采纳（误杀风险） |
| [cv-superding/WorkBuddy-Switch2api](https://github.com/cv-superding/WorkBuddy-Switch2api) | 网络错误（`code=-1`）被标 `needs_relogin` → 8 个号全废 | **反例**，写进本文件第 7 条 |

## 9. 代码落点

| 能力 | 位置 |
|---|---|
| 三态求证 / 节流 / 成功落盘 | `internal/upstream/verify.go` |
| 死因枚举、RT 判死标记、计数与复活 | `internal/pool/{state,transition,entry,persist}.go` |
| 401 求证接入、AT 豁免、面板刷新语义 | `internal/server/handler.go`（`penalizeSessionDead` / `handleRefreshSessionDead`）、`internal/server/search.go` |
| 保活豁免与 RT 标记 | `internal/scheduler/scheduler.go` |
| 手动刷新 / 单号重登 / 导入与移除护栏 | `internal/panel/{panel,login,import}.go` |
| 合并写 | `internal/auth/auth.go`（`SaveAtomicMerged`） |

## 10. 测试与回滚

- `go test ./...` 覆盖：三态判据表、求证落盘、节流、单次不禁用、三振禁用、
  `Inconclusive` 不计数、AT 豁免、旧 state 迁移、合并写保留手写键、重登 uid 校验/就地改写、
  导入默认跳过、移除需 force。
- 回滚：`pool.verify_session_on_401=false`（热生效）即回到「401 直接计数」的旧行为；
  其余改动（分型、豁免、面板重登）都是增量能力，不回退也能安全并存。

## 11. 已知边界

- 真吊销无法自动恢复（见 §6.3）；
- 上游 RT 自身也有到期时间（JWT `exp`），本项目当前不解析它做预警（列入后续）；
- 多进程同时刷新同一账号仍靠「采纳磁盘新 token 重试一次」兜底，跨进程文件锁未做
  （同类也只是进程内锁，最坏丢一次并发更新）。
