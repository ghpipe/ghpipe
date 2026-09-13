# ghpipe

ghpipe 是用 Go 重写的 aipipe：一个静态编译的单二进制 CLI，配合随二进制内嵌的 Skills，用 GitHub Issue / PR / Review / Checks 作为唯一任务账本，用 Owner 签发的受签会话约束 Developer、Reviewer、Orchestrator、Owner、automation 五类角色，只执行原子、可核验、可恢复的操作。它不启动任何模型客户端，不提供常驻服务，不维护第二套任务数据库。

当前状态：**设计阶段**。本仓库目前只有设计方案，尚无实现代码。

- **产品方案（先读这个）**：[docs/product.md](docs/product.md) —— 能干什么、业务流程怎么走、人需要做什么
- 技术设计：[docs/design.md](docs/design.md) —— 模块划分、配置、命令面、授权与分发
- 待确认决策：[docs/design.md §17](docs/design.md#17-待确认决策清单)
- 开发交接（换工具后从这里开始）：[docs/handoff.md](docs/handoff.md)

## 它能干什么

让 AI 编码助手在同一个仓库里，按「开发 → 独立验收 → 合并 → 收尾」的固定流程，把一个 GitHub Issue 自动做成已合并、可自证的代码：

1. **一个任务一条命令跑完**——主会话只编排，不写代码、不代替验收。
2. **开发和验收必须是两个独立 Agent**——两个主体密钥、两个 GitHub App，验收绑定当前 SHA。
3. **越权在工具层被拒绝**——签名会话 + 角色能力白名单 + 仓库/分支/PR/SHA 绑定。
4. **GitHub 就是账本**——阶段标签、Review、Checks、合并提交、清理声明，没有第二份状态。
5. **done 是核验出来的**——main CI、元数据、分支清理全部对上才叫完成。
6. **装完即用、三平台一致**——npm 一条命令，macOS / Linux / Windows 行为一致。

设计依据是 aipipe-template 的当前实现（CLI 0.8.2，`35400f4`）及其中的 `skills/`、`references/`，以及 cc-connect 的 npm 分发实现；不采用 aipipe 的历史设计文档作为依据。ghpipe 与 aipipe 不保持命令名、配置格式或资源布局的兼容，也不提供自动迁移。

## 计划中的形态

```bash
# 安装（包名 @ghpipe/cli，命令名 ghpipe；包装层按平台下载对应二进制）
npm install -g @ghpipe/cli

# 项目接入（生成 .ghpipe/ 资源与项目配置，Owner 受签执行）
ghpipe init repo --repo OWNER/REPO --apply

# 日常只读交接状态
ghpipe status --issue 42 --role developer --json
```

## 目录约定（目标形态）

| 内容 | 位置 |
|---|---|
| Go 源码 | `cmd/`、`internal/` |
| 随二进制内嵌的 Skills 与模板 | `resources/` |
| npm 包装层 | `npm/` |
| 方案与设计文档 | `docs/` |
| 发布流水线 | `.github/workflows/`、`Makefile` |

npm 包名是 `@ghpipe/cli`（作用域由 npm 组织 `ghpipe` 持有），安装后的命令是 `ghpipe`；不发布占位版本，首个功能版本随 release 流水线一起发布。命名与分发设计见 [docs/design.md §13](docs/design.md)。
