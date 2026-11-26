# 配置参考

**单一事实来源**：字段的完整默认值与结构见 [`config.example.json`](../config.example.json)；加载、校验与热生效逻辑见 `cmd/server/config.go`、`cmd/server/main.go`。根目录 [README 配置说明](../README.md#配置说明) 提供逐字段语义与注意事项。本文只维护分组索引和容易踩错的约束，避免第三套完整副本。

## 加载顺序与安全底线

1. 内置默认值；
2. `-config` 指定的 JSON；
3. 非空 `WB2A_*` 环境变量覆盖。

保存时走“深合并 + 原子替换”，保留未知键。未识别的枚举值应显式报错，而不是静默回落默认；修改后以实际启动日志 / 面板为准。

- `api_key` 为空 = **网关与面板都不鉴权**。公网或共享网络下必须设置随机长串。
- `config.json`、`auths/`、`data/` 都可能含敏感信息，不能提交。
- `auth_dir`、`state_file` 的相对路径按进程工作目录解析；容器部署要对齐 `/app` 挂载。

## 字段分组

| 分组 | 主要字段 | 说明 |
|---|---|---|
| 监听与凭据 | `listen`、`api_key`、`auth_dir`、`state_file`、`dns_servers` | 装配期字段改动通常需重启 |
| 入站服务器 | `server.read_timeout` | 覆盖请求读取（含 body 上传）；`0` = 不限制 |
| 面板 | `panel.package_detail_limit` | 积分构成页默认展开包数 |
| 日志 | `logging.request_archive_*`、`logging.request_client_info` | 请求元数据 JSONL，不含正文 / Authorization |
| 冷却 | `cooldown.soft_rate`、`cooldown.soft_rate_max` | 软限流基数与指数退避封顶 |
| 排程 | `schedule.*` | 六类 hourly 任务、余额刷新、补领清扫、账号级抖动 |
| 上游 | `upstream.*` | 超时、身份模式、UA、设备 token、IP 透传 |
| 功能开关 | `features.*` | 指纹清洗与 system→user 搬移兜底 |
| 模型 | `models.hidden`、`models.id_style`、`models.realm_name_prefix` | 列表过滤、域前缀写法、显示名 |
| 提示词 | `prompt.mode`、`prompt.file` | `passthrough` / `custom` / `append` |
| 账号池 | `pool.*` | 并发、熔断、降权、积分保底、跨域回退、401 求证 |
| 会话粘性 | `session_sticky.*` | 绑定 TTL 与 GC |
| Redis | `upstash.*` | 可选状态镜像；空 = 纯内存 |

## 容易踩错的字段

### `pool.verify_session_on_401` / `pool.session_verify_interval`

- 默认 `true` / `60s`，热生效。
- 401 先用 refresh 端点求证：Alive / Dead / Inconclusive；只有 Dead 计入连续失败。
- 出问题时设为 `false` 可退回“401 直接计数”的旧行为。

### `upstream.identity_mode`

- 默认空串（运行时按 `capture` 处理）；合法值 `capture` / `legacy`（`compat` 是 legacy 的兼容别名）。
- 拼错会启动失败，不会静默回落。
- 热生效；`legacy` 用于逐字节回退旧出站形态。

### `schedule.jitter_minutes`

- 默认 `45`；显式 `0` 关闭。
- 只影响定时排程，不影响面板手动“立即签到 / 上报 / 保活”。
- 任务按服务端本地时区解释，裸机部署请显式设置 `TZ`。

### 请求体上限

可配置的 `server.max_body_mb` 已移除，但代码仍有固定安全网：

- `/v1/chat/completions`、`/v1/responses`：**64 MiB**；超限网关返回 `413 request_too_large`。
- `/v1/search/messages`：**1 MiB**。

这不是“无上限”。多图 / 长上下文排障时以代码常量为准：`internal/server/handler.go` 的 `maxChatBodyBytes`、`internal/server/messages.go` 的 `messagesBodyLimit`。

### 超时边界

| 字段 | 作用 |
|---|---|
| `upstream.timeout_seconds` | 短 RPC 总时长（刷新 / 签到 / 余额 / 模型列表） |
| `upstream.header_timeout_seconds` | 聊天 SSE 首字节前；空 = 回落 timeout |
| `upstream.idle_timeout_seconds` | 聊天 SSE 流中空闲；不是总时长 |

聊天流使用 `Timeout=0` 的专用 client，**没有总时长上限**；不要用 `idle_timeout_seconds` 推断长输出会被总超时掐断。

## 修改配置时

```bash
# JSON 语法
python -m json.tool config.example.json >/dev/null

# 文档 / 示例 / README 一致性（仓库脚本，无第三方依赖）
node scripts/check-docs.js
```

新增配置字段时，同一提交内至少更新：

- `cmd/server/config.go` 的结构体与 `Default()` / `normalize()`；
- `config.example.json`；
- 根 README 的字段速查；
- 本文分组或“容易踩错”说明（若涉及运维语义）。