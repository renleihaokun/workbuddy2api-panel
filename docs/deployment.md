# 部署与升级

本文覆盖两种部署形态的可执行步骤。根 README 的 [部署运维](../README.md#部署运维) 保留发版原理、镜像构建细节与工具脚本说明。

## 0. 共同前置条件

```bash
git clone https://github.com/renleihaokun/workbuddy2api-panel.git
cd workbuddy2api-panel
cp config.example.json config.json
# 编辑 config.json：把 api_key 改成随机长串，并确认 TZ
openssl rand -hex 24
```

- `api_key` 为空 = 网关与面板都不鉴权。
- `auths/`、`data/`、`config.json` 均属敏感数据，不能提交、不能打进公共镜像。
- 生产环境必须有 HTTPS 反向代理或只监听内网 / 本机。

## 1. Docker Compose（推荐）

```bash
# 启动 / 重建
PUID="$(id -u)" PGID="$(id -g)" docker compose up -d --build

# 查看状态与日志
docker compose ps
docker compose logs -f --tail=100
```

首次启动配置与目录必须存在：`docker-compose.yml` 以文件 bind mount 挂载 `config.json`；若该文件不存在，Docker 可能创建同名目录，导致容器启动失败。先 `cp config.example.json config.json`。

挂载目录权限导致 `auths/*.json.tmp: permission denied` 时，优先让容器使用宿主机当前 uid/gid（上面的 `PUID` / `PGID`），或按 README FAQ 将挂载目录 chown 给 `10001:10001`。

**健康检查语义**：`/healthz` 是账号池 readiness，不是进程 liveness。首次登录前返回 503 是预期行为；不要把空池的 unhealthy 当成启动失败。

升级：

```bash
git fetch --all --prune
git checkout <目标 tag 或 commit>
PUID="$(id -u)" PGID="$(id -g)" docker compose up -d --build
docker compose ps
```

回滚：切回上一版本 tag 后重建。`./auths`、`./data`、`./config.json` 是宿主目录，`docker compose down` 不会删除它们；但执行 `down -v` 前必须确认没有需要保留的卷数据。

## 2. 单文件二进制 + systemd

Release 包里的二进制文件名为 `wb2api`；升级时请复制 / 重命名为 `wb2api.new`，不要直接覆盖正在运行的 `wb2api`。

约定示例（可按实际服务调整）：

- 代码 / 二进制目录：`~/workbuddy2api-panel`
- 二进制：`~/workbuddy2api-panel/wb2api`
- 服务：`workbuddy-panel.service`
- 配置：`~/workbuddy2api-panel/config.json`

### 2.1 发布前校验

```bash
sha256sum wb2api.new
strings wb2api.new | grep -F '<appVersion>'
```

### 2.2 干跑（不替换、不重启）

```bash
EXPECT='<sha256>' VER='<appVersion>' NEW=./wb2api.new BIN=./wb2api DRY_RUN=1 \
  bash ./deploy-wb2api.sh
```

### 2.3 正式滚动更新

```bash
sudo systemd-run --unit=wb2api-deploy --collect \
  env EXPECT='<sha256>' VER='<appVersion>' \
  bash ./deploy-wb2api.sh

cat deploy-last-status
cat deploy-<时间戳>.log
```

脚本行为：

- 校验新二进制 sha256 与版本串；
- 备份当前二进制为 `wb2api.bak-<时间戳>`；
- 原子替换（同目录 `mv`）；
- `systemctl restart workbuddy-panel`；
- 轮询 `/healthz`，失败自动回滚到备份并再次重启；
- 结果写入 `deploy-last-status` 与逐次日志。

前提：当前运行的服务确实使用脚本里的 `BIN` 与 `SERVICE`。如果生产布局不同，显式传 `BIN=/path/to/wb2api SERVICE=...`，并在干跑后确认路径。

### 2.4 健康检查与回滚边界

`/healthz` 在空池或所有账号不可用时返回 503，这是 readiness 设计。因此：

- 新版本发布后若账号池本来为空，部署脚本会判失败并回滚；首次部署请先完成登录，或在维护窗口临时使用其他探针；
- 回滚只恢复**二进制**，不自动恢复外部状态文件；需要回滚配置或数据时按备份人工处理；
- 脚本不执行数据库迁移，也不删除 `auths/`、`data/`。

## 3. 时区与定时任务

裸机 / systemd 部署必须显式设置 `TZ`：

```ini
# /etc/systemd/system/workbuddy-panel.service.d/50-timezone.conf
[Service]
Environment=TZ=Asia/Shanghai
```

```bash
sudo systemctl daemon-reload
sudo systemctl restart workbuddy-panel
```

否则 `[9, 21]` 等小时会按系统时区解释，看板的小时 / 日桶也会错位。已有 UTC 桶数据需要先停服再迁移：

```bash
python3 scripts/migrate_tz.py <data-dir>          # 演练，不写入
python3 scripts/migrate_tz.py <data-dir> --apply  # 先停服后再执行
```

日桶无法直接平移，脚本会拒绝并提示人工折叠。

## 4. 发布前检查清单

- [ ] 目标 tag 与 `cmd/server/main.go` 的 `appVersion` 一致（去掉 `-panel`）。
- [ ] Release 包含 `checksums.txt`，本地已校验。
- [ ] 已确认服务器使用的服务名 / 二进制路径。
- [ ] 已备份 `auths/`、`data/state.json`、`config.json`（或确认现有备份可用）。
- [ ] 干跑通过；正式执行后 `deploy-last-status` 为 `SUCCESS`。
- [ ] 回滚路径已验证（至少知道上一版本 tag / 备份二进制位置）。