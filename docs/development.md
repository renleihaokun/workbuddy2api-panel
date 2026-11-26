# 开发与测试

本文给本地开发和提交前检查的最短路径。业务行为、错误分类与架构细节见根 [README.md](../README.md)。

## 工具链

- Go：`go.mod` 声明 `go 1.22.5`；Docker 构建使用 `golang:1.23-alpine`。
- Node：用于面板前端语法检查与 `scripts/check-docs.js`；仓库无前端构建步骤，前端资源通过 `go:embed` 打包。
- Python：仅脚本 / 诊断工具需要；不是主服务运行依赖。
- Bash：`login.sh`、`signin.sh`、`credit.sh`、`deploy-wb2api.sh`；生产脚本应保持 LF 换行。

## 本地运行

```bash
cp config.example.json config.json
# 设置随机 api_key；本机自用也建议不要留空
go run ./cmd/server -config config.json
```

浏览器打开 `http://127.0.0.1:7863/panel/`。若没有 `config.json`，首次启动会自动生成并打印随机 `api_key`。

## 必跑检查

```bash
gofmt -l cmd internal
go vet ./...
go test ./...
node --check internal/panel/app.js
node --check internal/panel/mobile.js
node scripts/check-docs.js
bash -n login.sh signin.sh credit.sh deploy-wb2api.sh
```

CI 的 `go-binaries` 在 Ubuntu 上额外跑 `go test -race ./...`；本地 Windows 不要求复现 race，但涉及 pool / scheduler / auth 并发路径时应依赖 CI。

## 构建核心二进制

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wb2api ./cmd/server
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o signin_bin ./cmd/signin
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o login ./cmd/login
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o credit ./cmd/credit
```

`cmd/hdrprobe`、`cmd/trial` 是诊断 / 辅助工具，不随 Release 的五平台包分发；需要时单独 `go run`。

## 前端约束

- 无构建链：`index.html` 内联样式 + `app.js` / `mobile.css` / `mobile.js`，全部 embed。
- 手机 / 平板断点为 **1000px**：`mobile.css`、`mobile.js` 的 `MOBILE_MQ` 与日志页签 `matchMedia` 必须一致。
- 禁止内联事件处理器；CSP 为同源脚本。
- 改视觉后跑 `node --check` 与 `go test ./internal/panel`；涉及布局时至少检查 320 / 768 / 1000 / 1001 / 1440。
- `docs/ui-v*` 是历史视觉归档，不是当前设计令牌的事实来源。

## 配置与文档

新增或修改配置字段时：

1. 更新 `cmd/server/config.go` 的结构体、默认值与校验；
2. 更新 `config.example.json`；
3. 更新 README 字段速查与 [configuration.md](configuration.md)；
4. 跑 `node scripts/check-docs.js`。

`scripts/check-docs.js` 会检查关键事实漂移（旧版本号、手机断点、请求体上限、示例配置关键字段、docs 本地链接等）。它不是通用 Markdown linter；新增校验项请优先针对已经真实漂移过的契约。

## 变更与提交

- 提交前看 `git status`，确认没有 `config.json`、`auths/`、`data/`、备份包或本机绝对路径被意外加入。
- 生产事故修复优先补回归测试；不能自动化的行为需要写明手工验收步骤。
- 发布流程见 [releases.md](releases.md)，生产部署见 [deployment.md](deployment.md)。