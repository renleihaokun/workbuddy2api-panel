# 仓库工作规范

本文件是仓库级开发与发布规范。生产机连接信息、本机路径等环境相关配置不写入仓库；本机可另建 `AGENTS.local.md`（已被忽略）。

## 项目边界

- 这是个人自用的 CodeBuddy / WorkBuddy 网关与面板；不要把它改成面向公众的多租户商业服务。
- 涉及上游账号、凭据、风控与协议的操作必须优先保证“可回滚、可观察、不误伤账号”。
- 不引入批量注册、账号交易、额度售卖、卡密分发等实现。

## 修改前

1. 先读 `README.md` 对应章节与 `docs/` 的专题文档；不要只依据历史调研快照。
2. 涉及配置字段时，先查 `cmd/server/config.go` 与 `config.example.json`，确认默认值与热生效边界。
3. 涉及部署 / 发布时，先读 `docs/deployment.md`、`docs/releases.md`。
4. 不要在生产机试错；服务器操作必须遵守用户提供的生产环境约束。

## 代码与测试

- 保持 Go 标准库优先、无新增依赖除非确有必要。
- 提交前至少：

```bash
gofmt -l cmd internal
go vet ./...
go test ./...
node --check internal/panel/app.js
node --check internal/panel/mobile.js
node scripts/check-docs.js
bash -n login.sh signin.sh credit.sh deploy-wb2api.sh
```

- CI 会跑 `go test -race ./...`；并发路径改动要依赖或补相应回归测试。
- 前端无构建步骤：`index.html` / `app.js` / `mobile.css` / `mobile.js` 都通过 `go:embed` 打包；手机断点为 1000px。
- 不提交 `server.exe`、`wb2api`、`login` 等构建产物。

## 配置与文档

同一提交内保持以下一致性：

- `cmd/server/config.go`（结构体、默认值、校验）；
- `config.example.json`；
- README 配置表；
- `docs/configuration.md`（分组 / 易错项，不是字段全表副本）；
- 如涉及 API、部署、发布，同步对应 docs 页面。

`scripts/check-docs.js` 是当前的最小契约检查；发现新的真实漂移模式应加检查项，不要只靠人工记忆。

## 文档规范

- README 是完整技术手册；`docs/` 是按任务拆分的入口。
- 研究 / 视觉归档必须写日期、性质（快照 vs 当前规范）和“以什么为准”。
- 不写行号作为长期稳定锚点，改用文件 + 符号名。
- 不在仓库文档写真实服务器地址、绝对本机路径、token、私钥、账号邮箱 / UID 或个人环境值；用占位符。
- 发布包只带必要文档；敏感研究或设计稿若不宜公开，应放在私有位置而不是让整个 `docs/` 被忽略。

## 发布与生产

- 版本唯一来源是 `cmd/server/main.go` 的 `appVersion`；README 不重复维护“当前版本”。
- 发布前确认 tag 与 `appVersion` 一致、Release 有 `checksums.txt`。
- 单文件部署先 `DRY_RUN=1` 校验，再执行；失败按 `deploy-last-status` / 日志回滚。
- 生产变更前备份 `auths/`、`data/`、`config.json`；不要用过期导出覆盖仍然有效的 refresh token。
- 重启或改动其他服务、系统配置、安装依赖、删除数据、批量操作，必须先得到用户确认。

## 提交前检查

```bash
git status --short
git diff --check
git diff --stat
node scripts/check-docs.js
```

确认没有：

- `config.json`、`auths/`、`data/`、`*.bak-*`、部署日志、备份包；
- 本机绝对路径、服务器地址、真实 token / 私钥；
- 未跟踪的临时研究文件混入发布提交。