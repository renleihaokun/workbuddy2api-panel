# 文档索引

本目录存放可随仓库分发的技术文档。根目录 [README.md](../README.md) 是完整使用与行为手册；这里的页面负责导航和分工，避免再把所有历史、研究和运维细节继续堆到首页。

## 先看哪一篇

| 目标 | 文档 |
|---|---|
| 第一次部署、添加账号、验证 | [getting-started.md](getting-started.md) |
| 查配置字段、默认值、热生效规则 | [configuration.md](configuration.md) / [config.example.json](../config.example.json) |
| Docker Compose、单文件部署与升级回滚 | [deployment.md](deployment.md) |
| 生产备份、日志、时区、故障排查 | [operations.md](operations.md) |
| OpenAI / Responses / 搜索端点 | [api.md](api.md) |
| 本地构建、测试、文档校验 | [development.md](development.md) |
| 版本号、tag、Release、GHCR | [releases.md](releases.md) |

## 专题归档

| 文档 | 性质 | 维护约定 |
|---|---|---|
| [outbound-identity.md](outbound-identity.md) | 当前出站身份形态、开关与代码落点 | 代码改动导致头族/UA/开关变化时同步更新 |
| [refresh-token-lifecycle.md](refresh-token-lifecycle.md) | refresh token 生命周期与 401 处置 | 账号处置状态机变化时同步更新 |
| [outbound-fingerprint-research.md](outbound-fingerprint-research.md) | 2026-11-25 调研快照 | 保留历史证据；现行口径以 outbound-identity 为准 |
| [competitive-research.md](competitive-research.md) | 2026-11-11 竞品与路线图快照 | 不是现行 API 清单；落地状态以代码/README 为准 |
| [ui-v3/README.md](ui-v3/README.md) | v1.20.0 面板视觉归档 | 历史截图，不承诺对应当前面板 |
| [ui-v4/README.md](ui-v4/README.md) | v1.23 前后面板响应式与交互改造记录 | 设计决策记录 |

## 文档边界

- **README**：产品定位、完整能力、配置速查、核心行为语义、FAQ、稳定代码锚点。
- **docs/**：按任务拆分的入口与专题资料。入口页只写稳定摘要并链接到 README 的对应章节，不复制大段实现细节。
- **研究/视觉归档**：开头必须写日期、性质（快照 vs 当前规范）和“以什么为准”。过期结论可以保留，但必须显式标注。
- **敏感信息**：`auths/`、`data/`、`config.json`、服务器地址与本机绝对路径不进入文档。生产机操作规范放在仓库根目录 `AGENTS.md`，环境相关值用占位符或本机 untracked 文件。

## 修改文档时

```bash
node scripts/check-docs.js
bash -n deploy-wb2api.sh
```

配置结构或 `config.example.json` 变更时，同一个提交内同步以下两处：

- [configuration.md](configuration.md) 的字段索引；
- [config.example.json](../config.example.json) 的示例键。