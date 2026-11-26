# 发布与版本

本文说明版本的单一事实来源与发布门禁。详细原理见根 README 的 [发版（自动）](../README.md#发版自动)。

## 单一事实来源

| 事实 | 来源 |
|---|---|
| 服务版本 | `cmd/server/main.go` 的 `appVersion` |
| Git tag | `appVersion` 去掉 `-panel`，例如 `1.28.0-panel` → `v1.28.0` |
| 发布产物 | GitHub Releases + `checksums.txt` |
| 容器镜像 | `ghcr.io/renleihaokun/workbuddy2api-panel`，tag 由 workflow 生成 |
| 文档中的“当前版本” | 不再手工维护；查 Releases 或 `appVersion` |

## 正常发版流程

1. 修改 `cmd/server/main.go` 的 `appVersion`；
2. 跑本地检查（见 [development.md](development.md)）；
3. 提交并推送到 `main`；
4. `.github/workflows/auto-release.yml` 检测 `cmd/server/main.go` 改动，读取版本号；
5. tag 不存在则创建并推送；
6. 显式 dispatch `go-binaries.yml` 与 `docker-ghcr.yml`；
7. 检查 Release 资产与 Actions 结果。

自动流程的幂等边界：tag 已存在时跳过，不会覆盖已发布资产。补发 / 演练可手动 dispatch workflow 到目标 tag 引用。

## CI 断言

`go-binaries` 会检查：

- `go test -race ./...`；
- tag 名等于 `appVersion` 去掉 `-panel`（`-ci` 演练 tag 豁免）；
- 二进制内确实包含 `appVersion` 字符串；
- 打包命名不带 `-panel` 后缀。

发布前本地至少确认：

```bash
go vet ./...
go test ./...
node scripts/check-docs.js
bash -n deploy-wb2api.sh
```

## 产物

| 渠道 | 内容 |
|---|---|
| GitHub Release | Windows amd64 zip；Linux amd64 / arm64 tar.gz；macOS amd64 / arm64 tar.gz；`checksums.txt` |
| GHCR | 多架构镜像 `linux/amd64`、`linux/arm64`；tag 含版本、主次版本、sha 等 |

Release 资产含二进制、`config.example.json`、README 与 `deploy-wb2api.sh`；不含 `auths/`、`data/`。生产部署脚本既可从 Release 包取，也可从源码仓库同步到服务器。

## 回滚

- Git tag 回退不能覆盖已发布资产；回滚以部署上一版本二进制 / 镜像为准。
- 单文件部署使用 `deploy-wb2api.sh`；发布前先 `DRY_RUN=1` 校验，再正式执行。
- 生产回滚步骤与备份边界见 [deployment.md](deployment.md) 与 [operations.md](operations.md)。

## 文档同步

涉及以下变化时，发布提交必须同步对应文档：

- 配置字段 / 默认值：`config.example.json` + README 配置表 + [configuration.md](configuration.md)；
- API 端点 / 限制：README API 章 + [api.md](api.md)；
- 发布流程 / 产物：本文 + README 部署运维；
- 界面断点 / 视觉：`docs/ui-*` 归档或对应设计记录。