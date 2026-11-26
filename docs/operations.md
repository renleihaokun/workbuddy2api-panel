# 运维手册

生产运维以“先观察、再变更、可回滚”为准。根 README 的 [错误分类与账号处置](../README.md#错误分类与账号处置)、[定时任务](../README.md#定时任务)、[安全与合规](../README.md#安全与合规) 是详细语义来源。

## 1. 备份与恢复

### 必须备份

| 数据 | 路径 | 说明 |
|---|---|---|
| 账号凭证 | `auths/` | 明文 access / refresh token，权限应为 `0600` |
| 池状态 | `data/state.json` | 冷却、禁用、积分、粘性等持久状态 |
| 配置 | `config.json` | 含 `api_key`、设备 token、Upstash 凭据 |
| 用量 / 归档 | `data/` 其余文件 | 取决于是否需要保留历史与价格覆盖 |
| 二进制 / 部署脚本 | `wb2api`、`deploy-wb2api.sh` | 回滚时需要 |

### 最少备份命令

```bash
STAMP="$(date +%Y%m%d-%H%M%S)"
tar czf "wb2api-backup-${STAMP}.tar.gz" \
  auths data config.json
chmod 600 "wb2api-backup-${STAMP}.tar.gz"
```

备份包内有明文 token，不要上传网盘、提交 Git 或放进公共制品库。

恢复时：

1. 先停服务；
2. 恢复 `auths/`、`data/`、`config.json`；
3. 检查权限（凭证 `0600`，目录仅运行用户可写）；
4. 启动并检查 `/status`、日志与账号状态。

## 2. 日常检查

```bash
# 服务与最近日志（systemd 示例）
systemctl status workbuddy-panel --no-pager
journalctl -u workbuddy-panel -n 200 --no-pager

# 容器部署
docker compose ps
docker compose logs --tail=200
```

面板「账号池」「通道健康」「运行日志」优先用于业务判断：

- 账号是否 `disabled` / `needs_relogin` / `refresh_token_dead`；
- 是否有模型级限流锁池；
- 通道拒绝、限流、流中断是否突增；
- 实际服务域与跨域回退是否符合预期。

## 3. 时区与桶迁移

任务按**进程本地时区**解释；看板小时桶也按服务端本地时区分桶。裸机部署必须显式设置 `TZ`：

```ini
# /etc/systemd/system/workbuddy-panel.service.d/50-timezone.conf
[Service]
Environment=TZ=Asia/Shanghai
```

```bash
sudo systemctl daemon-reload
sudo systemctl restart workbuddy-panel
```

已积累 UTC 小时桶时：

```bash
systemctl stop workbuddy-panel
python3 scripts/migrate_tz.py data --apply
systemctl start workbuddy-panel
```

先不带 `--apply` 演练。脚本只平移 `h:` 小时桶；遇到日桶会拒绝，需人工折叠。

## 4. 账号不可用时的处置

| 现象 | 含义 | 操作 |
|---|---|---|
| `session_dead` / 待重登 | refresh token 被明确作废 | 面板“重登”，就地改写凭证并保留 device_token |
| `banned` / 11140 异常行为 | 账号级封禁 | 重登无效，走官方申诉 |
| `refresh_token_dead` 但 AT 仍可用 | 还能服务，但无法续期 | 尽快重登；AT 到期后会退出池 |
| 单次 401 / 12153 | 不足以判定吊销 | 默认先求证；查 `pool.verify_session_on_401` 与日志 |
| 429 / 5xx / 网络错误 | 不是吊销证据 | 不应触发账号禁用 |

详细状态机见 [refresh-token-lifecycle.md](refresh-token-lifecycle.md)。

## 5. 配置变更

在线保存通过面板“配置”页完成，走深合并 + 原子替换。保存后检查返回的“需要重启字段”。

- 能热生效的字段（如 `soft_rate`、`pool.*` 大部分、`features.*`、`prompt.mode`）应观察下一轮请求；
- 装配期字段（如 `listen`、`dns_servers`、`auth_dir`、`state_file`）需重启；
- 修改 `api_key` 后面板与客户端都要更新；
- 修改 `pool.verify_session_on_401=false` 是 401 求证的回滚闸。

配置字段索引见 [configuration.md](configuration.md)。

## 6. 常见故障

### `/healthz` 返回 503

这是 readiness：没有 healthy 且未占满的账号，或所有可用账号都不可服务。先看 `/status` 与面板账号池，不要直接判定进程崩溃。容器空池时 unhealthy 是预期行为。

### 登录时报 `permission denied`

容器内 `app` 用户（uid 10001）不能写宿主挂载目录。用 `PUID=$(id -u) PGID=$(id -g) docker compose up -d --force-recreate`，或把 `auths/`、`data/`、`config.json` 的属主调整为运行用户。

### 请求体超限

- chat / responses：网关 64 MiB 安全网，超限直接 `413 request_too_large`；
- search：1 MiB；
- 不要按“网关无上限”排查，以 `internal/server/handler.go` 与 `internal/server/messages.go` 的常量 / 测试为准。

### 定时任务整点偏 8 小时

进程时区仍为 UTC。按第 3 节设置 `TZ=Asia/Shanghai`；已有桶数据需停服迁移。

### 升级后仍加载旧前端

面板静态资源带内容哈希 ETag 与 `Cache-Control: no-cache`。先硬刷新 / 确认二进制版本；若仍异常，检查反向代理是否自行缓存了 `app.js`。

## 7. 上游目录与端点取证（`cmd/catalogprobe`）

模型列表"突然少了一堆/多了一堆"、或怀疑上游改了接口时，先取证再改配置：

```bash
# 在网关所在机器上执行（与 auths/ 同机）；只发 GET，不发推理请求，不打印 token
go run ./cmd/catalogprobe -auth-dir ./auths                 # CN 域
go run ./cmd/catalogprobe -auth-dir ./auths -realm global    # global 域
go run ./cmd/catalogprobe -auth-dir ./auths -baseline data/catalog-cn.json   # 与上次指纹对比
go run ./cmd/catalogprobe -auth-dir ./auths -write-baseline data/catalog-cn.json
```

看三样东西：

1. **每路的 status** —— `404` = 路径被改；`401/403` = 鉴权/风控拦了；`500` + HTML = 边缘层。
   global 的 `/console/enterprises/personal/models` 自 2026-11 起长期 500，属已知降级（只用 v3 三路）。
2. **信封键**（`data` 段的键）—— 上游新增/删改结构会先在这里显形，即使本版本还没解析那些段。
3. **`disabled` 清单** —— 上游标成下架的模型。两条目录路（`/v3/config` 与企业端点）都按它剔除；
   过滤时进程日志会打 `INFO: [upstream] v3/config 过滤 disabled 模型 N 个…`。

`-public` 会额外发 3 次**无鉴权** GET（`/v3/config`、企业模型端点、`/v2/chat/completions`），
只用来区分"路径没了"与"要鉴权"，默认不开。

## 8. 回滚原则

1. 二进制回滚：使用最近的 `wb2api.bak-<时间戳>` 或上一 Release 包，替换后重启。
2. 配置回滚：恢复备份的 `config.json`，不要用旧示例覆盖生产密钥。
3. 状态回滚：`data/state.json` 可能已被新版本写入新字段；回滚前先备份当前状态，避免旧版本读取时丢字段。
4. 凭证回滚：不要用过期的导出覆盖 `auths/`；refresh token 会轮换，旧文件可能已失效。