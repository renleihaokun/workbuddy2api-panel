# 快速开始

本文只覆盖“从零到第一次可用”的最短路径。完整能力、行为语义与 FAQ 见根目录 [README.md](../README.md)。

## 前置条件

- 一个或多个本人有权使用的 CodeBuddy 账号。该网关使用 OAuth 登录，不需要也不应提供密码。
- Docker + Docker Compose，或一个可运行的 `wb2api` 单文件二进制。
- 仅从源码构建时需要 Go ≥ 1.22；CI 构建镜像使用 Go 1.23。
- 选择 `api_key` 随机长串。示例配置中的 `""` 表示**不鉴权**，只适合完全隔离的本机环境。

## 方式 A：Docker Compose

```bash
git clone https://github.com/renleihaokun/workbuddy2api-panel.git
cd workbuddy2api-panel

cp config.example.json config.json
# 编辑 config.json，把 api_key 改成随机长串：
openssl rand -hex 24

docker compose up -d --build
docker compose ps
```

打开 `http://127.0.0.1:7863/panel/`，用面板“添加账号”完成登录。账号凭证落盘到 `auths/`（容器内 `0600`），运行状态落盘到 `data/`。

> `/healthz` 是 readiness：**账号池为空时返回 503 是预期行为**。首次启动后先完成登录，再把它当作可用性检查。容器自带的 `HEALTHCHECK` 也使用同一判据；若只看容器状态，空池时 unhealthy 不代表进程没启动。

## 方式 B：单文件

从 [Releases](https://github.com/renleihaokun/workbuddy2api-panel/releases) 下载对应平台包，校验 `checksums.txt` 后运行：

```bash
./wb2api -config config.json
# 浏览器打开 http://127.0.0.1:7863/panel/
```

第一次启动若没有 `config.json`，程序会自动生成一份并写入随机 `api_key`；日志只打印一次。也可以先复制 `config.example.json` 再手动填写，但务必替换空密钥。

## 添加账号

推荐使用面板“添加账号”：生成授权链接 → 浏览器完成 OAuth → 凭证自动落盘并热加载，无需重启。

Linux / macOS 也可以使用 `login.sh`，需要 `bash` 与 `python3`；Windows 请优先使用面板或 WSL。

## 验证

```bash
# 有可用账号后，/healthz 才应返回 200
curl -s http://127.0.0.1:7863/healthz

curl -s http://127.0.0.1:7863/v1/models \
  -H "Authorization: Bearer <你的 api_key>"

curl -s http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer <你的 api_key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

## 首次部署后必须做

1. 确认 `config.json` 不在 Git 中，且权限仅限运行用户可读。
2. 备份 `auths/` 与 `data/state.json`；不要把备份提交到仓库。
3. 若端口暴露到内网 / 公网：设置非空 `api_key`，并置于 HTTPS 反向代理后。
4. 裸机部署显式设置 `TZ`，避免定时任务和看板小时桶错位；见 [operations.md](operations.md)。
5. 阅读 [安全与合规](../README.md#安全与合规) 与根目录使用声明。