# ghpipe 设计方案（Go 重写）

状态：**待确认，未实施**。本文件是技术设计依据，确认后才进入开发。
**阅读顺序**：先看 [产品方案](product.md)（做什么、卖点、业务流程），再看本文件（怎么实现）。产品方案里「复杂度分层」中的**核心层**对应本文必须实现的部分，增强层与可选项可以晚于首个可用版本。
依据：aipipe-template 当前实现（CLI 0.8.2，`main` 35400f4，2026-09-13）及其 `skills/`、`references/`；分发形态参考 `/Users/ws/code/cc-connect/npm`。
目标仓库：`github.com/ghpipe/ghpipe`（当前为空仓库）。本文所有产物都落在该仓库。

---

## 目录

- [0. 摘要](#0-摘要)
- [1. 目标与非目标](#1-目标与非目标)
- [2. 保留的产品契约](#2-保留的产品契约)
- [3. 需要重新设计的地方](#3-需要重新设计的地方)
- [4. 总体架构](#4-总体架构)
- [5. 配置设计](#5-配置设计)
- [6. 授权与会话](#6-授权与会话)
- [7. GitHub 层](#7-github-层)
- [8. Git 层](#8-git-层)
- [9. 命令面](#9-命令面)
- [10. 业务模块](#10-业务模块)
- [11. 接入体验：App 与 Skills](#11-接入体验app-与-skills)
- [12. 自动化与 CI](#12-自动化与-ci)
- [13. 分发与发布](#13-分发与发布)
- [14. 测试策略](#14-测试策略)
- [15. 实施计划](#15-实施计划)
- [16. 风险](#16-风险)
- [17. 待确认决策清单](#17-待确认决策清单)
- [18. 缺口清单与处置](#18-缺口清单与处置)

---

## 0. 摘要

ghpipe 是一个静态编译的单二进制 CLI。它把「任务账本」留给 GitHub，把「角色授权」留给 Owner 签发的受签会话，把「规范」留给随二进制内嵌的 Skills，本身只做三件事：**受签的原子写操作、只读的事实核验、可恢复的失败处理**。

与 aipipe 相比，形态上有三个根本变化：

1. **运行时依赖从「Python + git + gh + openssl」收缩为「git + 网络」**。CLI、CI 自动化、质量门禁全部由同一份二进制执行；gh 子进程、openssl 子进程、Python 解释器都不再需要。
2. **分发从「pip wheel」改为「GitHub Release 静态二进制 + npm 包装层」**，按平台自动下载并校验 sha256，安装路径与 cc-connect 同构。
3. **模块划分与配置重新设计**，不保留 `project.json`、`compatibility.json`、`github`/`api` 透传命令、`scripts/*.py`、`error_reporting`、`preferences`、`metadata.mode` 等历史包袱；目录统一为 `.ghpipe/`，命令统一为 `ghpipe`。

保留的是**产品契约**：五类角色与能力白名单、八步生命周期、唯一 `Closes #N` 关联、按来源与 SHA 绑定的必需检查、独立验收、固定 SHA 合并、结算式清理、写回执与「未知写不重放」。

### 0.1 摘要决策表

| 议题 | 决策 | 一句话理由 |
|---|---|---|
| 语言与形态 | Go 1.24+ 单二进制，`go:embed` 内嵌全部 Skills/模板/参考 | 去运行时依赖，装完即用 |
| GitHub 访问 | 进程内 HTTP（`net/http`）自研传输与分页契约，**不依赖 `gh`，也不引入 go-gh** | 凭据本就自签，不需要 gh 的登录发现；分页完整性是本项目的核心契约，必须自持 |
| 会话签名 | 会话用 **Ed25519**（标准库）；GitHub App JWT 仍用 **RS256**（GitHub 强制） | 去掉 openssl 依赖，密钥更小更快 |
| CI 执行体 | 生成的 workflow 用**固定版本**的 ghpipe 二进制执行（npm 包装层或 Release 直连，均校验 sha256） | 与本地同一份实现，避免第二套代码 |
| 配置 | `.ghpipe/config.json` 单一 schema；宿主凭据在外；资源指纹写在 `.ghpipe/manifest.json` | 一份配置一个职责，去除遗留字段 |
| 结果契约 | 所有命令统一 `--json` 结果信封 + 固定退出码语义（0/1/2/3） | 机器可判定「确定失败」与「结果未知」 |
| 写恢复 | 统一 `journal`：每个写意图一个文件，状态机 `pending → confirmed / refused / reconciled` | 未知写只允许正向证据回读，不允许重放 |
| 平台 | **macOS / Linux / Windows 三平台**，x86-64 与 arm64 为必需目标；32 位目标（linux/386、linux/arm v7、windows/386）作为可选构建产物 | 目标用户跨三平台；平台差异（锁、权限、符号链接、换行符）统一下沉到 `hostfs` 抽象，并用三平台 CI 验证，不再假设 POSIX |
| 命名 | 二进制 `ghpipe`、项目目录 `.ghpipe/`、分支 `ghpipe/issue-N`、工作流 `.github/workflows/ghpipe-*.yml` | 重写即重命名，不背兼容包袱 |
| App 接入 | 一条 `ghpipe init app` 驱动全流程；浏览器只保留 GitHub 强制的两次点击（创建 App、安装），其余全自动 | GitHub 没有创建 App、创建 installation、创建 PAT 的 API，这是唯一可行的边界（§11.1） |
| Skill 接入 | 对齐 larksuite/cli 策略：内容内嵌 + `ghpipe skills read` 兜底；工具目录**链接优先、拷贝兜底**；版本/技能更新一律「重新安装即覆盖 + 清理退出项 + 回读校验集合」；用户级只放 1 个 `ghpipe` 路由技能 | 链接让正本唯一、更新即生效；拷贝覆盖 Windows 等不支持链接的环境；内嵌内容保证任何情况下 Agent 都读得到（§11.2） |
| 项目定制 | `.ghpipe/skills/` 就地复写 + `.ghpipe/skills.local/` 本地层，用 manifest 哈希区分 managed/modified/custom | 让项目能改官方流程，同时让升级知道哪些是官方、哪些是本地（§11.3） |

### 0.2 规模基准

| 项 | aipipe 现状 | ghpipe 目标（估算） |
|---|---|---|
| CLI 实现 | 28 个文件 6,123 行 Python | 约 10,000–13,000 行 Go（含嵌入资源、统一结果层、skills 与 app 接入子系统、三平台 `hostfs` 抽象） |
| 自动化副本 | 6 个模块 1,604 行，随项目复制 | 0（复用同一二进制） |
| 测试 | 30 个文件 363 个用例 5,897 行 | 约 300–400 个 Go 用例（含协议夹具与端到端） |
| 人读文档 | `references/` 977 行 + Skills 130 行 | 重写为内嵌 `references/` + Skills（规模相当） |
| 分发 | pip wheel（未发布 PyPI） | Release 资产 + npm 包 |

---

## 1. 目标与非目标

### 1.1 目标

1. **零运行时依赖安装**：`npm install -g @ghpipe/cli`（包名见 §13.5；装完后的命令名是 `ghpipe`）后即可在任意有 `git` 的机器上运行；不要求 Python、不要求 `gh`、不要求 openssl。
2. **同一份实现跑在三处**：开发者宿主、GitHub Actions 自动化、目标项目 CI 质量门禁，都由同一个二进制执行，不存在「模板副本与源码不一致」这一类问题。
3. **可核验**：任何写操作都能回答三个问题——是谁签发的、绑定到哪个任务与 SHA、结果是否确定。
4. **可恢复**：结果不确定时永不重放，只允许原主体用正向远端证据回读。
5. **可移除**：目标项目里 ghpipe 的文件全部可枚举、可预览、可退休，不污染业务目录。
6. **可分发**：GitHub Release 资产 + npm 包装层 + 校验和 + 镜像回退，版本可固定、可校验、可离线安装。
7. **三平台一致体验**：macOS、Linux、Windows 使用同一套命令、同一份配置、同一套 Skills；平台差异由内部抽象吸收，用户命令与文档不因平台而分叉。

### 1.2 非目标

- 不实现模型路由、客户端启动器、常驻调度服务、任务数据库、缓存或队列。
- 不替代 GitHub 的裁决：分支保护、必需检查、审批人数、合并可行性都由 GitHub 判定，ghpipe 只做可读证据的核验与来源绑定。
- 不做跨仓库原子操作、跨机器分布式锁、跨主机协调。
- 不提供 aipipe → ghpipe 的自动迁移器；不读取 `.aipipe/`、不兼容旧配置、不提供旧命令别名。
- 不探测或启动任何外部 AI 客户端；`preferences`（工具/模型偏好）这一概念整体删除。
- 不承诺 GitHub Enterprise Server、组织 SSO（SAML）场景；不单独验证 Windows Server Core、WSL 嵌套容器等特殊宿主。
- 32 位目标（linux/386、linux/arm v7、windows/386）只提供构建产物与安装脚本，不进入三平台测试矩阵；遇到平台专属缺陷时按可选目标处理。

### 1.3 与 aipipe 的关系

重写而不是移植。原项目中被认为实现良好的部分直接借鉴语义并重新表达：受签会话与能力白名单、写回执、分页完整性校验、生命周期投影、元数据调节、发布幂等、质量门禁的 bootstrap/strict 区分、GitHub App manifest 注册流程、独立验收与固定 SHA 合并。被重新设计的是模块边界、配置格式、命令面、依赖与分发。

---

## 2. 保留的产品契约

这一节是「不许在重写中丢失」的清单，全部来自当前实现与 Skills 的实际行为。实现时每条都要有对应测试。

### 2.1 角色与能力

五个工作角色，能力白名单固化在代码中（`internal/trust/capabilities.go`），签发时只能缩小、不能放大：

| work_role | credential_role | 能力（capability） |
|---|---|---|
| developer | developer | `read` `identity` `run` `config` `commit` `push` `pr.create` `pr.edit` `comment` `token` |
| reviewer | delivery | `read` `identity` `run` `review` `comment` `token` |
| orchestrator | delivery | `read` `identity` `claim` `merge` `cleanup` `token` |
| owner | owner | `read` `identity` `config` `init` `token` `trust` `publish` `release` `metadata` |
| automation | delivery | `read` `identity` `metadata` |

不变量：

- Developer、Reviewer、Orchestrator 必须是不同 subject（不同主体密钥），验收者不能由实现者担任，合并者不能由验收者担任。
- 会话绑定 repo、项目 realpath、Issue、精确分支；Reviewer 与具备 `merge`/`cleanup` 的 Orchestrator 还必须绑定 PR 与完整 SHA。
- 工作角色（署名）与凭据角色（GitHub App）是两层，identity 文件只能声明、不能授权。
- 会话最长 24 小时；`token` 能力只能续签绑定角色的凭据，不能延长会话。

### 2.2 生命周期八步

阶段是**从 GitHub 事实投影出来的**，不是可以手工推进的状态机：

| 阶段 | 事实 | 下一责任人 |
|---|---|---|
| ready | 有 Issue，无活动分支/PR | 主会话核对放行与依赖，只领取一个 |
| active | 存在 `ghpipe/issue-N` 分支或 Draft PR | Developer |
| review | 非 Draft PR，未合并 | Reviewer |
| changes-requested | 独立 Review 要求返修 | 原 Developer |
| closing | 已合并，但 main CI / metadata / 清理证据未齐 | 主会话安排剩余原子动作 |
| done | 合并提交 CI、metadata、远端分支与本地/跟踪 ref 全部确认 | 结束，不再开发 |
| cancelled | Issue 与全部关联 PR 关闭且未合并 | 报告取消，保留分支与提交 |

`blocked` 是叠加标签；`pending` / `failed` / `unknown` 是**操作结果**，不是业务阶段。

### 2.3 关联与证据规则

- Issue↔PR 的唯一关联是**默认分支上、同仓库、正文中独占一行的 `Closes #N`**，通过 GitHub 的 `CROSS_REFERENCED_EVENT` 发现，不靠分支命名猜。
- 正文全文扫描关闭关键字（含代码块、引用块、仓库限定名、URL 形式），出现第二个关闭引用即阻断，绝不静默降级为 ready。
- 一个 Issue 只有一个活动 PR；一个 Issue 只占一个交付里程碑；PR 的 milestone 强制为空（避免件数重复计数）。
- 同一对象只保留一个阶段标签，`aipipe:blocked` → `ghpipe:blocked` 作为叠加保留；PR 继承 Issue 的非阶段标签。
- 必需检查按 **context + integration_id + head SHA** 三重绑定；同名 check-run 与 commit status 同时存在时两者都必须成功；`skipped`/`neutral` 保留原 conclusion，不表述为「测试已执行通过」。

### 2.4 合并门禁

合并必须同时满足：

1. GitHub 原生 `reviewDecision == APPROVED`；
2. 当前 SHA 的、来源绑定的必需检查全部成功；
3. 原生 `mergeStateStatus == CLEAN`；或 `BLOCKED` 但经审计可证明「质量规则无 bypass + 唯一 update 规则给调用者 PR-only 资格 + 非 Draft + 无冲突 + head 已包含最新 base」；
4. 存在与所有开发主体都不同的 Reviewer 主体的、绑定当前 SHA 的批准，**且该批准的 GitHub 身份必须是配置的 Delivery App 机器人**（`user.login == <delivery slug>[bot]`，并带 reviewer 身份块）——用 Developer App 或任何其他账号提交的批准一律不计入；单有 GitHub 原生「1 个批准」不足以放行；
5. 合并方式仅 squash，禁用 `--admin`/`--auto`，squash 正文保留全部原始提交作者与署名 trailers；
6. 写后回读确认 `merged == true` 且 merge SHA 与写响应一致；已 merged 的重复调用只确认成功并进入幂等清理，永不重复 merge。

### 2.5 分支与署名不变量

- 开发分支只能是从最新干净默认分支创建一次的 `ghpipe/issue-N`；禁止在 `main`/`master`/配置默认分支/`origin/HEAD` 指向分支或 detached HEAD 上开发。
- push 必须推送「当前分支名 = 目标分支名 = 已签分支」的精确 `<sha>:refs/heads/<branch>`，origin 必须是 HTTPS 的 `github.com`。
- 提交的 Author 是 `工具 / 模型`，Committer 是实际执行者并附角色；邮箱为 `<uuid>@ghpipe.invalid`（不冒用个人或 bot 账号）。
- 只对本次 git 子进程注入 `GIT_AUTHOR_*` / `GIT_COMMITTER_*`，不改全局或仓库 Git 配置，不自动暂存，不重写历史。
- `attribution.required` 时，push 前按「远端默认分支实际 SHA」为边界核验未发布提交的作者与 trailers 完整性；浅克隆、历史不完整、基线失效一律停止。

### 2.6 失败语义（贯穿全部命令）

| 情形 | 语义 | 处理 |
|---|---|---|
| 写前校验失败 | 确定失败，无副作用 | 修条件后重试 |
| HTTP 400/401/403/404/405/409/422 明确拒绝 | 确定失败 | 清回执，修条件后重试 |
| 网络错误、超时、5xx、响应损坏、写后回读不一致 | **结果未知** | 保留 pending，只允许原主体正向证据回读 |
| 只读读取不完整（分页重复、cursor 异常、total_count 不足） | 结果未知 | 不把部分事实用于写入 |

---

## 3. 需要重新设计的地方

| # | 现状 | 问题 | ghpipe 的设计 |
|---|---|---|---|
| 1 | 通过 `gh` 子进程访问 GitHub | 需要用户装 gh ≥2.48；错误分类靠 stderr 正则（`could not resolve host` 等）；每次调用一个进程 | 进程内 `net/http`；错误分类用 `*url.Error` / `net.DNSError` / `x509` 类型；注入 `http.RoundTripper` 即可测试 |
| 2 | `github`/`api` 透传 + argv 二次解析（`identity.value`、`remaining != expected`） | 授权边界靠字符串重组，脆弱且难测 | 类型化子命令（`pr create`、`pr review`…），每个命令显式声明目标与载荷字段，禁止任意 flag 透传 |
| 3 | openssl 子进程做 RSA 签名 | 额外依赖；签名错误只能拿到退出码 | 会话与信任根用 Ed25519（`crypto/ed25519`）；仅 GitHub App JWT 用 RSA（`crypto/rsa`，PKCS#1 v1.5，GitHub 强制 RS256） |
| 4 | Python 运行时：CLI、6 个 automation 模块、quality 脚本 | 三份执行体；模板副本与源码可能漂移；目标项目 CI 需要 Python | 一个二进制；automation 入口 `ghpipe metadata workflow`；质量门禁 `ghpipe quality check` |
| 5 | `project.json` 混杂 `error_reporting`、`preferences`、`metadata.mode`、遗留 `required_check`+`integration_id` | 字段语义重叠，部分字段实际不起作用（上报能力已收窄、preferences 从不使用） | 精简 schema：只保留 repository/apps/ci/commands/quality/attribution/execution |
| 6 | `compatibility.json` + 硬编码 `RETIRED_RESOURCES` 指纹表 | 退休许可写死在二进制里，新增可删指纹必须改代码 | `.ghpipe/manifest.json` 记录「本工具安装过的路径 + 内容 sha256」，撤回只允许删自己装过且未被修改的文件 |
| 7 | `--result-json`（仅 review/merge）与 status JSON 两套契约 | 机器解析规则不一致 | 所有命令统一 `--json` 信封 + 统一退出码 |
| 8 | `receipts`：`execution-<digest>.json` 与 `execution-pending-<op>.json` 双文件名约定 | 命名即接口，跨命令校验散落各处 | `state/journal/` 下单一状态机，索引与查询由一个包负责 |
| 9 | 每次写操作都要 `--identity FILE` | 重复参数与一类常见错误 | identity 文件路径可由 `subject_id` 推导，缺省自动使用；文件不存在则明确报错要求先 `identity create` |
| 10 | 资源随 pip wheel 分发，`scripts/*.py` 兼容包装 | 安装产物与源码树两套路径判断 | `go:embed` 单一来源；删除全部脚本包装 |
| 11 | 测试大量打桩 `subprocess.run` | 断言耦合进程细节 | 传输与 Git 都走接口：`github.Transport`、`gitx.Runner`，测试用内存实现或 `httptest` |
| 12 | 命名/路径 `aipipe`、`.aipipe/`、`aipipe/issue-N` | 重写却沿用旧名会长期歧义 | `ghpipe`、`.ghpipe/`、`ghpipe/issue-N`、`ghpipe-*.yml` |
| 13 | App 接入依赖用户手工操作：读文档、开网页、复制 App ID/私钥/installation ID、按 Enter 继续 | 接入门槛高，易错，失败后难以恢复 | 一条 `ghpipe init app` 驱动：manifest 自动提交 → 回调捕获 → 自动换私钥 → 自动引导安装 → 自动校验并写配置（详见 §11.1） |
| 14 | Skill 只存在于项目内 `.aipipe/`，靠用户手工建立各工具入口 | 多工具（codex/pi/zcode/opencode/claude）各自约定不同，用户容易漏装或装错；也容易把同一技能装到多个作用域造成同名重复 | 项目级 `.agents/skills` 链接（拷贝兜底，覆盖式同步）+ 用户级单条 `ghpipe` 路由技能 + root `AGENTS.md` 兜底，由 `skills sync` 统一维护（§11.2） |
| 15 | 资源升级与项目定制二元对立：受管文件被改就整体拒绝 | 项目想复写官方 Skill 只能 fork，升级时无法判断“谁是官方” | 用 `manifest.json` 哈希区分 managed/modified/custom/retired，三方可视化合并（§11.3） |
| 16 | 实现默认 POSIX：`fcntl.flock`、`pwd` 模块、`0600`、`/dev/null`、LF 检出 | 无法在 Windows 上运行，也无法承诺三平台一致 | 平台差异收敛到 `hostfs`（锁 / 原子替换 / 密钥保护 / 链接与拷贝 / 文本规范化），CI 在三个 runner 上跑同一套测试（§4.5、§12.4） |

---

## 4. 总体架构

### 4.1 形态

```
                     ┌──────────────────────────────┐
   ghpipe 二进制 ────▶ │ cmd/ghpipe (CLI 入口)         │
   （内嵌资源）       │  ├─ internal/cli   命令树/信封 │
                     │  ├─ internal/trust 会话与能力   │
                     │  ├─ internal/journal 写回执     │
                     │  ├─ internal/github 传输/分页   │
                     │  ├─ internal/gitx   Git 执行    │
                     │  └─ internal/{status,metadata,  │
                     │      policy,quality,publish,…}  │
                     └──────────────────────────────┘
                                │            │
                    GitHub REST/GraphQL    git（本地/推送）
```

### 4.2 包结构与依赖方向

依赖只能向下，禁止跨层反向引用；`internal/lifecycle` 与 `internal/attribution` 是纯函数包，不得引入 I/O。

| 包 | 职责 | 依赖 |
|---|---|---|
| `cmd/ghpipe` | 入口、信号处理、退出码 | `internal/cli` |
| `internal/cli` | 命令树、全局参数、结果信封、错误到退出码的映射 | 其余全部（唯一下行入口） |
| `internal/hostfs` | 平台抽象：文件锁、原子替换、密钥文件保护检查、链接与目录拷贝、控制台编码、（可选）ACL | — |
| `internal/project` | 项目发现（`--project`/`--config`/向上查找）、`config.json` 校验、路径解析 | — |
| `internal/credentials` | 仓库外凭据注册表、角色凭据读取、子进程环境清洗 | `project` `hostfs` |
| `internal/trust` | 信任根、会话签发/校验、能力表、角色矩阵 | `credentials` |
| `internal/journal` | 写回执状态机、未知写清点、正向证据协调入口 | `project` |
| `internal/attribution` | 纯格式：trailers、PR 正文身份块、`Closes #N` 语法 | — |
| `internal/lifecycle` | 纯投影：阶段计算、结算事实聚合规则 | — |
| `internal/github` | HTTP 传输、分页契约、GraphQL 契约、错误分类、端点模板、App JWT/installation token | `credentials` |
| `internal/gitx` | git 命令封装、分支守卫、远端核验、凭据助手、push、cleanup 的 Git 部分 | — |
| `internal/task` | 单检出任务绑定与文件锁 | `gitx` `project` |
| `internal/status` | 只读交接状态装配 | `github` `lifecycle` `metadata` |
| `internal/checks` | 必需检查事实（rules + rollup） | `github` |
| `internal/policy` | 有效规则审计、质量规则判定、合并门禁 | `checks` `github` |
| `internal/metadata` | 事实收集、标签/里程碑期望与同步、workflow 事件路由 | `github` `lifecycle` `attribution` |
| `internal/quality` | 质量门禁（bootstrap/strict） | `gitx` `project` |
| `internal/publish` | 批次与单维护 Issue 的预览/发布/放行 | `github` `attribution` |
| `internal/cleanup` | 合并后本地清理与结算声明 | `gitx` `github` `lifecycle` |
| `internal/setup` | init：repo/app/checks/metadata/resources/seed | 全部 |
| `internal/appreg` | GitHub App manifest 注册与本地回调 | `github` |
| `internal/resources` | 内嵌资源清单、diff 预览、指纹升级与退休 | `project` |
| `internal/doctor` | 交接前置检查与连通性诊断 | `github` `policy` `checks` |
| `internal/result` | 结果信封与错误分类的类型定义 | — |
| `internal/version` | 版本、资源版本、兼容性门禁 | `project` |

### 4.3 关键接口

只为「需要替换实现」的地方定义接口，不为抽象而抽象：

```go
// 测试注入用；生产实现是 *http.Client 包装
type Transport interface {
    Do(ctx context.Context, req Request) (Response, error) // Request 含 Method/Path/Query/Body/Headers
    Pages(ctx context.Context, req Request) ([]Page, error) // 分页契约，逐页返回并校验
}

// git 执行；测试注入脚本化实现
type Runner interface {
    Run(ctx context.Context, dir string, env []string, args ...string) (Result, error)
}

// 时间与随机源，便于会话/回执的确定性测试
type Clock interface{ Now() time.Time }
type Nonce interface{ New() string }
```

`internal/github` 的生产实现只依赖 `*http.Client`（可注入自定义 `RoundTripper` 做录制/回放），不做任何重试：失败就是失败，未知就是未知。

平台能力也走接口，保证除 `hostfs` 外没有任何包直接调用 `syscall`：

```go
// 平台抽象（internal/hostfs，按 GOOS 分文件实现）
func Lock(path string) (func() error, error) // unix: flock；windows: LockFileEx
func Replace(target string, data []byte, mode os.FileMode) error // 临时文件 + 原子替换，windows 带占用重试
func CheckSecret(path string) (Protection, error) // Protection ∈ {posix_0600, user_profile_acl, unknown}
func EnsureEntry(target, dst string, mode Mode) (Mode, error) // Mode ∈ {auto, link, copy}；auto = 先链接、失败转拷贝
func CopyTree(src, dst string) error                          // 覆盖式拷贝（逐文件临时文件 + 原子替换）
func InspectEntry(dst string) (EntryState, error)             // EntryState ∈ {linked, copied, missing, foreign}
func NormalizeText(data []byte) []byte             // CRLF → LF，用于资源哈希与复写判定

// 路径策略与加固打开（借鉴 larksuite/cli 的 internal/vfs/localfileio）
func ValidateInputPath(p string) (string, error)   // windows: 拒绝 UNC/设备命名空间/保留设备名；所有平台：拒绝非本地命名空间
func OpenValidated(p string) (*os.File, error)     // unix: O_NOFOLLOW|O_NONBLOCK + 拒绝多链接 + SameFile；windows: SameFile
func AtomicWrite(p string, data []byte, mode os.FileMode) error      // 覆盖语义：temp + rename（windows 带重试）
func ExclusiveWrite(p string, r io.Reader, mode os.FileMode) error   // 不覆盖语义：temp + link 提交（不同名即失败）
```

`EnsureEntry` 是 Skills 落盘的唯一写入路径：`auto` 先尝试符号链接（正本唯一、更新即生效），失败自动转为覆盖式拷贝（Windows 无开发者模式、文件系统不支持链接等）。`CopyTree` 只用于拷贝模式与资源落盘：逐文件写临时文件再原子替换，覆盖前先确认目标是 ghpipe 写过的。`InspectEntry` 用于状态判定。`NormalizeText` 保证同一份资源在三个平台得到相同哈希（§4.5）。

**覆盖写与独占写是两种语义，不能混用**：`AtomicWrite`（rename 提交）会无条件替换目标，适合配置、回执、资源文件；`ExclusiveWrite`（link 提交）在目标已存在时失败、且只在内容完整后才让目标名字出现，适合 journal 的 pending 条目、App 私钥、主体公钥注册表这类「绝不能覆盖半个文件」的场景。这条区分的理由是参考实现的注释里写明的：rename 会无条件替换，而 `O_EXCL` 直写会让不完整文件在复制期间就可见。

### 4.4 三类目录边界

| 边界 | 位置 | 是否入 Git | 内容 |
|---|---|---|---|
| 目标项目 | `<project>/.ghpipe/` | 是（`state/` 除外） | `config.json`、`manifest.json`、`AGENTS.md`、`SKILL.md`、`skills/`、`references/`、`templates/`、`plans/` |
| 目标项目运行态 | `<project>/.ghpipe/state/`、`<project>/.ghpipe/skills.local/` | 否（由工具写 `.gitignore`） | `journal/`、`actors/`、`task.json`；本地专属 Skill 层 |
| 目标项目工具暴露 | `<project>/.agents/skills/`、`.claude/skills/`、`.zcode/skills/`（按工具表），以及根 `AGENTS.md` 的受管块 | 链接/拷贝与受管块入 Git；`.gitignore` 视工具约定 | 指向 `.ghpipe/skills/<name>` 的链接（不可用时为覆盖式拷贝），由 `ghpipe skills sync` 维护（§11.2） |
| 目标项目 GitHub 固定路径 | `.github/workflows/ghpipe-*.yml`、`.github/ISSUE_TEMPLATE/ghpipe-bug.yml` | 是 | 只有 GitHub 要求固定位置的文件放在这里：工作流与用户提交用的 Issue Form（§10.11.1、§12.1），其余一律留在 `.ghpipe/` |
| 宿主私有 | `os.UserConfigDir()/ghpipe/`：macOS/Linux 为 `~/.config/ghpipe/`，Windows 为 `%APPDATA%\ghpipe\` | 否 | `credentials.json`、`execution-authority.pub`、`owner/{keys,apps,trust,subjects}/`、`tokens/<owner>/<repo>/<role>.token` |
| 宿主工具入口 | `~/.agents/skills/ghpipe/`（唯一路由技能），以及 `~/.pi/agent/skills/ghpipe`、`~/.zcode/skills/ghpipe` 等工具侧入口 | 否 | 由 `ghpipe skills sync --user` 链接/拷贝/覆盖/移除；工具路径表在 `~/.config/ghpipe/tools.json` |

### 4.5 跨平台约束（Windows 是首要差异源）

所有平台差异集中在下表，处理方式已固化到 `hostfs`，业务包不感知平台：

| 差异 | Windows 行为 | 处理方式 |
|---|---|---|
| 文件锁 | 没有 `flock` | `hostfs.Lock` 双实现：unix 用 `flock`，Windows 用 `LockFileEx`（经 `syscall.NewLazyDLL("kernel32.dll")` 调用，不引入第三方依赖）；锁文件统一放 `.ghpipe/state/` |
| 密钥权限 | 没有 POSIX mode，`chmod` 只切只读位 | 密钥一律放用户配置目录（Windows `%APPDATA%` 默认仅本用户可读）；`check` 返回 `user_profile_acl` 而不是谎称 `0600`；外部凭据文件在 Windows 上只校验「位于检出外 + 位于用户 profile 内」 |
| 原子替换 | `MoveFileEx(REPLACE_EXISTING)` 可覆盖，但目标被占用会失败 | `hostfs.Replace`：临时文件 + rename；Windows 上遇占用做有限退避重试，仍失败则明确报错，不写半成品 |
| 符号链接 / 目录链接 | 创建需开发者模式或管理员权限，且各工具与杀软行为不一致 | 与 larksuite/cli（`skills add`）同样的策略：**链接优先、拷贝兜底**。`hostfs.EnsureEntry(auto)` 先建相对符号链接，失败则覆盖式拷贝；两种模式都由状态文件 `.ghpipe/state/skills-state.json` 记录（`mode`、目标、内容哈希、CLI 版本），更新时按记录覆盖 ghpipe 自己写过的入口，非 ghpipe 的路径报告 `foreign` 并保持只读（`--force` 才覆盖）。根解仍是技能内容内嵌在二进制里：`ghpipe skills read` 不依赖任何落盘（§11.2.7） |
| 换行符 | 检出可能被 `core.autocrlf` 转成 CRLF | 资源哈希与复写判定统一 `NormalizeText`（CRLF→LF）后比较；写入始终用 LF；文档建议（不强制）用 `.gitattributes` 固定 `.ghpipe/**` 为 LF |
| 路径语义 | 反斜杠、盘符、`\\?\` 长路径 | 仓库相对路径（git 语义）统一用 `path` 包；本地文件系统用 `filepath`；git 输出先 `filepath.FromSlash` |
| 禁用 Git hooks | `/dev/null` 不存在 | 用一次性空目录作为 `core.hooksPath`，三平台一致，不写 `/dev/null` |
| 控制台编码 | 默认代码页可能非 UTF-8 | 启动时尽最大努力切到 UTF-8 代码页；`--json` 输出恒为 UTF-8 且不含 ANSI 转义 |
| 信号 | 无 SIGTERM；Ctrl+C 为 `Interrupt` | 只处理 `os.Interrupt`（含 CTRL_BREAK）；进程被强杀后靠 `journal` 恢复，不依赖信号做清理 |
| 可执行位 | 无 | 质量门禁的可执行位判断只读 `git ls-tree` 的树内模式，不看工作区 |
| 大小写敏感性 | 文件系统不区分大小写 | 技能名与受管路径的冲突检测按大小写不敏感比较；仅大小写不同的同名技能一律拒绝 |

#### 4.5.1 从两个参考实现借来的跨平台做法

来源：`cc-connect`（同为 Go 单二进制 + npm 分发，有 `instance_lock.go`、`daemon/{systemd,launchd,windows,unsupported}.go`、`Makefile` 六平台矩阵）与 `larksuite/cli`（同类 CLI，有 `internal/vfs/localfileio/*` 的路径策略与加固打开）。

| 做法 | 出处 | ghpipe 采纳方式 |
|---|---|---|
| 平台专属文件 + 构建标签（`instance_lock.go` 用 `//go:build !windows`，另一份给 Windows） | cc-connect | `hostfs` 按 `_unix.go` / `_windows.go` 分文件，业务包不出现 `runtime.GOOS` 分支 |
| 不支持的功能给出可执行建议，而不是静默降级（`daemon/unsupported.go`：`use a process manager (e.g. nssm, pm2) instead`） | cc-connect | 例如 32 位目标、未知工具路径、无法建链接等场景，一律"明确报错 + 给出替代做法" |
| 锁文件里写入持有者信息（PID），冲突时报"哪个进程持有" | cc-connect | `.ghpipe/state/lock` 写入 `pid + subject_id + purpose`，冲突时报出持有者，便于判断是否是残留 |
| 候选目录列表 + 首个命中（Linux `XDG_DATA_HOME`→`~/.local/share`、macOS `~/Library/Application Support`、Windows `%APPDATA%`） | cc-connect | 工具发现目录与宿主数据目录按候选列表解析；配置目录支持 `GHPIPE_CONFIG_DIR` 覆盖，解析不到就明确报错而不是猜路径 |
| 配置文件里平台不支持的字段直接报错（如 `run_as_user` 在 Windows 上拒绝） | cc-connect | 项目配置里出现平台专属字段时同样拒绝并说明原因，避免"配了但没生效" |
| shell 选择按平台分支（`powershell.exe -Command` vs `sh -c`） | cc-connect | **不采纳**：ghpipe 一律用 argv 数组执行，自身不引入 shell（更少的平台差异）；技能脚本与项目命令若需要 shell，必须在 `commands` 里显式写 `["bash","-lc",...]` / `["powershell","-Command",...]` |
| 路径策略：Windows 拒绝 UNC、设备命名空间（`\\.\`、`\\?\`、`\??\`）、保留设备名（`CON`、`NUL.txt`），并拒绝非本地命名空间 | larksuite/cli | `hostfs.ValidateInputPath` 采用同一拒绝清单；仓库相对路径统一 `/` |
| "先校验路径、再打开句柄、用 `SameFile` 把结论绑回对象"（Unix 另加 `O_NOFOLLOW`、`O_NONBLOCK`、拒绝多链接文件） | larksuite/cli | 凭据、私钥、会话、资源文件全部走 `OpenValidated`；Windows 无 `O_NOFOLLOW`，靠 `SameFile`（卷 + 文件索引）兜底 |
| 覆盖写（temp+rename）与独占写（temp+link）分开，并写清原因 | larksuite/cli | 见 §4.3：配置/回执用 `AtomicWrite`，journal/密钥/主体注册表用 `ExclusiveWrite` |
| Windows 人类输出用 ASCII 回退（`[OK]`、`->`） | larksuite/cli | 采纳；`--json` 恒为 UTF-8 且不含装饰字符 |
| 二进制自更新时先改名 `.old` 再替换，崩溃可恢复 | larksuite/cli | 采纳到 `ghpipe update` 与 npm 包装层（§13.2、§13.6） |

两点需要如实说明：

1. **两个参考实现的 CI 都只在 Ubuntu 上跑**（larksuite/cli 26 个 job 全是 ubuntu，cc-connect 亦然），Windows/macOS 只有构建产物、没有测试。ghpipe 的三平台测试矩阵是**比两者更严**的要求，必须真的在 `windows-latest`/`macos-latest` 上执行，而不是只交叉编译。
2. 它们的跨平台一致性能成立，部分原因是**不用符号链接放置技能**（飞书用安装器的链接，Windows 走 `--copy`）或**不涉及文件权限语义**（cc-connect 的锁只在 `!windows` 下用 `flock`，Windows 另有实现）。ghpipe 与两者都不同：既要用链接（技能入口），又要管权限（凭据），所以 `hostfs` 的覆盖面比两者都大。

目录覆盖的边界（对应 D24）：`GHPIPE_CONFIG_DIR` 之类的环境变量只允许改变**非安全状态**的位置（`tools.json`、`skills-state.json`、缓存）；信任根 `execution-authority.pub` 与 `owner/` 下的密钥、信任记录、主体注册表**始终按 OS 用户配置目录解析**，不吃环境变量。理由很直接：能改信任根位置的环境变量，等于把提权入口交给环境。

三平台一致性由 CI 保证：`go test ./...` 在 ubuntu / macos / windows 三个 runner 上运行，平台专属用例用构建标签隔离（§12.4、§14）。

任何凭据、私钥、token 都不得落在项目检出内（结构与软件双重检查：路径包含关系 + 权限位）。

---

## 5. 配置设计

### 5.1 三层分离

| 数据 | 位置 | 入 Git | 备注 |
|---|---|---|---|
| 仓库绑定、App 编号、逻辑凭据引用、原生命令、质量模式、署名策略 | `<project>/.ghpipe/config.json` | 是 | 非秘密；不含 token |
| 逻辑引用 → 环境变量 / token 文件映射 | `~/.config/ghpipe/credentials.json` | 否 | 只存映射与过期时间 |
| App 私钥、会话签发私钥、主体私钥、短期 token、共享 App 信任记录 | `os.UserConfigDir()/ghpipe/owner/…`、`…/tokens/…` | 否 | 仅本用户可读：POSIX 为 0600/0700；Windows 依赖用户配置目录 ACL（`doctor` 如实报告 `protection=user_profile_acl`） |
| 任务进度、Review、CI、合并事实 | GitHub | — | 不建本地任务库 |
| 本工具安装过的资源指纹 | `<project>/.ghpipe/manifest.json` | 是 | 升级/退休的许可依据 |

### 5.2 `.ghpipe/config.json`

```json
{
  "schema_version": 1,
  "repository": "OWNER/REPO",
  "default_branch": "main",
  "quality": {"mode": "strict"},
  "apps": {
    "developer": {
      "app_id": 123456,
      "installation_id": 7654321,
      "slug": "owner-ghpipe-developer",
      "credential_ref": "OWNER/REPO/developer"
    },
    "delivery": {
      "app_id": 123457,
      "installation_id": 7654322,
      "slug": "owner-ghpipe-delivery",
      "credential_ref": "OWNER/REPO/delivery"
    }
  },
  "ci": {
    "required_checks": [{"context": "ghpipe-quality", "integration_id": 15368}],
    "workflow_path": ".github/workflows/ghpipe-quality.yml"
  },
  "commands": {
    "test": {"cwd": ".", "argv": ["npm", "run", "test"]},
    "build": {"cwd": ".", "argv": ["npm", "run", "build"]}
  },
  "attribution": {"required": true, "legacy_before": "40位完整SHA"},
  "execution": {"strip_env": ["CUSTOM_GITHUB_WRITE_TOKEN"]}
}
```

字段表（含与旧配置的对应关系）：

| 字段 | 类型 | 必填 | 说明 | 旧字段 |
|---|---|---|---|---|
| `schema_version` | int | 是 | 固定 `1`（ghpipe 自身的第一版） | `schema_version` |
| `repository` | `OWNER/REPO` | 调远端时必填 | 仓库绑定；改绑必须显式 `--rebind` 并清空 apps/ci/default_branch | `repository` |
| `default_branch` | string | 调远端时必填 | 必须与 GitHub 实际默认分支一致 | `default_branch` |
| `quality.mode` | `bootstrap` \| `strict` | 否（缺省 strict） | 只用于初始化放行判定，不是通用跳过开关 | `quality.mode` |
| `apps.{role}` | object | 用 GitHub 时必填 | `app_id` / `installation_id` / `slug` / `credential_ref`；developer 与 delivery 必须是不同 App | 同 |
| `ci.required_checks` | array | 用门禁时必填 | 每项 `{context, integration_id}`，context 唯一 | `ci.required_checks` |
| `ci.workflow_path` | path | 用门禁时必填 | 必须在 `.github/workflows/` 下 | 同 |
| `commands.*` | `{cwd, argv}` | 跑业务命令时必填 | argv 数组，禁止 shell 字符串；**`cwd` 必填且必须是项目内相对路径**（拒绝空串、`/`/`\` 开头、UNC、盘符 `C:`、任一段 `..`，判定不依赖宿主平台语义） | 同 |
| `attribution.required` | bool | 否 | 要求角色写操作携带 identity 并核验提交署名 | 同 |
| `attribution.legacy_before` | 完整 SHA | 否 | 只豁免仍为 HEAD 祖先的启用前历史 | 同 |
| `execution.strip_env` | string[] | 否 | 运行项目命令/测试时额外清除的环境变量 | 同 |

**删除的旧字段与理由**：

| 旧字段 | 处置 | 理由 |
|---|---|---|
| `error_reporting` | 删除 | 现实现已把它收窄为「配置不授予上传能力」的历史开关，默认关闭；新工具只做本地诊断输出 |
| `preferences`（planning/development/review 的工具与模型） | 删除 | 从不触发任何行为，只造成「配置即声明」的误解；实际工具/模型记录在 identity 与 GitHub 产物中 |
| `metadata.mode`（inline/workflow） | 删除 | 同步恒由 Owner/automation 能力执行，`inline` 已不再授予写权限 |
| `ci.required_check` + `ci.integration_id` | 删除 | 只保留 `required_checks` 数组，避免双写法与优先级歧义 |
| `compatibility.minimum_cli_version` | 迁移到 `manifest.json` | 与资源版本同属「工具自述」，不与项目业务配置混放 |
| `apps.*.credential_ref` 的默认值 `aipipe/<role>` | 删除 | 一律使用 `OWNER/REPO/<role>` 显式命名 |

### 5.3 `.ghpipe/manifest.json`（工具自述与资源指纹）

```json
{
  "schema_version": 1,
  "resource_version": "0.1.0",
  "minimum_cli_version": "0.1.0",
  "generated_at": "2026-09-13T12:00:00Z",
  "files": {
    "AGENTS.md": "sha256:…",
    "skills/ghpipe-develop/SKILL.md": "sha256:…",
    "references/lifecycle.md": "sha256:…"
  }
}
```

- 运行前取「运行版本」与 `minimum_cli_version` 的更严格值；不满足即停止。
- 资源版本与运行版本不一致时给出明确警告（不阻止只读命令）。
- 退休只允许删除 `files` 中记录、且当前内容哈希与记录一致、且仍在受管前缀（`skills/`、`references/`、`templates/`、`plans/`）内的文件；任何不在记录内或已被修改的内容一律保留并报告。

### 5.4 宿主凭据注册表

```json
{
  "credentials": {
    "OWNER/REPO/developer": {"kind": "token_file", "identity": "app", "path": "/outside/developer.token", "expires_at": "2026-09-13T13:00:00Z"},
    "OWNER/REPO/delivery": {"kind": "env", "identity": "user", "name": "GHPIPE_ORDERS_DELIVERY_TOKEN"}
  }
}
```

- 只允许 `env` 与 `token_file` 两种；App 私钥属于 Owner 材料，不进注册表。
- `identity` 取值 `app`（installation token，默认）或 `user`（PAT／用户 token，§11.1.4 路径 C）；它只用于 `doctor` 如实报告隔离来源（`apps` vs `accounts`），不授予任何额外能力。
- `token_file` 必须位于检出外；POSIX 上还要求权限 0600，Windows 上要求位于用户配置目录或用户 profile 内（无 POSIX mode，保护级别由 `doctor` 报告）；文件内容不得含空白。
- 未登记 `expires_at` 时本地无法证明有效期，由远端验证；认证失败不重试写入。

Owner 材料目录：

```text
~/.config/ghpipe/
├── credentials.json            # 逻辑引用注册表
├── tools.json                   # AI 工具 → 用户级/项目级 Skill 目录（可选，缺省用内置表）
├── execution-authority.pub     # Owner 信任根（固定路径，不接受环境变量或项目提供）
├── owner/
│   ├── keys/<app_id>.pem       # App 私钥（仅本用户可读）
│   ├── apps/<app_id>.json      # App 非秘密元数据 + 私钥路径
│   ├── subjects/<subject_id>.pub  # 已登记主体公钥（签发时免传参、可枚举、可吊销）
│   └── trust/<owner>/<repo>/<role>.json  # 共享 App 信任记录
└── tokens/<owner>/<repo>/<role>.token    # 短期 installation token（仅本用户可读）
```

### 5.5 配置写入规则

- `ghpipe config set` 只合并非秘密字段，保留未涉及字段；写盘统一走 `hostfs.Replace`（临时文件 + 原子替换，Windows 带占用重试），不直接 `os.Rename`。
- 改绑仓库需 `--rebind`，会清空 `apps`、`ci`、`default_branch`，不静默沿用旧安装。
- `commands` 的键只允许 `[A-Za-z0-9_.-]`；`npm run test:e2e` 是 argv 内容，键用 `e2e`。
- 写配置前必须通过分支守卫（有 `.git` 时），禁止在默认分支直接改配置。
- `ghpipe config validate --json` 输出机器可读校验结果；`ghpipe inspect --json` 输出运行版本、资源版本、入口路径、配置摘要（不含 token）。

---

## 6. 授权与会话

### 6.1 信任模型

```text
Owner（宿主隔离环境）
  ├─ 签发私钥（RSA→Ed25519，仅本用户可读，项目外）
  ├─ 信任根 ~/.config/ghpipe/execution-authority.pub（本机唯一，不可由环境变量/项目覆盖）
  └─ 为每个主体签发短期会话
        │
        ▼
主体（Developer / Reviewer / Orchestrator / automation / Owner）
  ├─ 自己的私钥（仅本用户可读，项目外）
  └─ 会话 JSON（含能力、绑定、有效期、nonce、签名）
        │
        ▼
ghpipe 每次执行前：验签 → 验期 → 验绑定（realpath/cwd/origin/分支/PR/SHA）→ 验能力
```

签名证明「这份授权由已安装的信任根签发，且调用者持有匹配的主体私钥」。它**不**证明模型名称、自然人身份或宿主真实角色；也不能阻止同一 OS 权限的程序读取私钥或直接操作 Git/GitHub。这条边界在文档中必须保持原样表述，不夸大。

### 6.2 会话结构

```json
{
  "schema_version": 1,
  "session_id": "uuid",
  "subject_id": "uuid",
  "subject_public_key": "base64-ed25519",
  "work_role": "developer",
  "credential_role": "developer",
  "root": "/abs/original/project",
  "repository": "OWNER/REPO",
  "issue": 42,
  "branch": "ghpipe/issue-42",
  "pr": null,
  "sha": null,
  "capabilities": ["read", "identity", "run", "config", "commit", "push", "pr.create", "pr.edit", "comment", "token"],
  "issued_at": "2026-09-13T12:00:00Z",
  "expires_at": "2026-09-13T13:00:00Z",
  "nonce": "uuid",
  "signature": "base64-ed25519-over-canonical-json"
}
```

校验规则（缺一即拒）：

| 规则 | 说明 |
|---|---|
| 字段集合精确匹配 | 未知字段即非法，避免凭据被塞进会话 |
| 角色配对 | `work_role` 与 `credential_role` 必须来自固定映射表 |
| UUID 规范化 | `subject_id` / `session_id` / `nonce` 必须是规范 UUID |
| root 绝对路径 | 必须与实际 cwd、`git rev-parse --git-common-dir` 推出的检出根一致 |
| 分支精确 | 非 owner/automation 角色必须满足 `branch == "ghpipe/issue-" + issue` |
| PR/SHA 绑定 | reviewer 及含 `merge`/`cleanup` 的 orchestrator 必须携带 PR 与完整 SHA |
| 能力子集 | `capabilities` 必须是角色白名单的非空子集且无重复 |
| 有效期 | 必须带时区、未过期、总时长 ≤ 24h |
| 签名 | 用 `execution-authority.pub` 验 Ed25519；主体公钥必须与 `--subject-key` 匹配 |
| 私钥位置 | `--subject-key` 必须在项目外；POSIX 上校验 0600，Windows 上校验位于用户 profile 内（`hostfs.CheckSecret`）；解析默认位置时**不读环境变量覆盖**（D24） |

### 6.3 能力与命令

| capability | 允许的命令 | 额外约束 |
|---|---|---|
| `read` | `status` `doctor --connectivity` `metadata`（只读）`task inspect` `journal list` `api GET` | 不改任何远端状态 |
| `identity` | `identity create/show` | 角色与凭据角色必须等于会话 |
| `run` | `run <name>` | 清洗环境变量，不注入写凭据 |
| `config` | `config set/validate` | 分支守卫 + 仓库一致 |
| `commit` | `commit` | 必须携带 identity；禁止在 merge/rebase/cherry-pick 中执行 |
| `push` | `push <branch>` | 分支名 = 已签分支；origin 为 HTTPS；署名核验 |
| `pr.create` / `pr.edit` | `pr create/edit` | head 必须等于已签分支；base 必须等于实际默认分支；正文必须含唯一 `Closes #issue` |
| `comment` | `pr comment` / `issue comment` | 目标编号必须显式且等于已签 Issue/PR |
| `review` | `pr review` | 必须 `--match-head-commit`，写 REST `commit_id`，写后回读 head |
| `merge` | `pr merge` | 仅 squash；合并门禁通过；写后回读 |
| `cleanup` | `cleanup` | 仅对已确认合并的同仓库 PR |
| `claim` | `task claim/handoff/release` | 只操作本机绑定 |
| `token` | `auth token` | 只续签绑定凭据角色 |
| `metadata` | `metadata --apply` | 只改标签/里程碑，不改 Review/merge |
| `publish` / `release` | `publish` `release --apply` | Owner 专属 |
| `init` | `init *` | 允许在绑定仓库上创建/接入；不越界到别的 repo |
| `trust` | `auth trust-app` | 只写仓库外信任记录 |

### 6.4 签发、续期、吊销

```bash
# 生成主体密钥（只输出公钥，不授予任何权限）
ghpipe execution keygen --output /protected/developer.pem

# 登记主体公钥（写入 ~/.config/ghpipe/owner/subjects/<uuid>.pub）
ghpipe execution enroll --subject-public-key /protected/developer.pub --subject-id UUID

# 安装信任根（拒绝覆盖不同公钥；轮转属于宿主管理操作）
ghpipe execution trust --signing-key /protected/owner-signing.pem

# 签发
ghpipe execution issue --signing-key /protected/owner-signing.pem \
  --subject-id UUID --work-role developer \
  --root /abs/project --repository OWNER/REPO --issue 42 \
  --minutes 60 --output /protected/developer-session.json

# 枚举与吊销（新增，用于轮转与审计）
ghpipe execution list
ghpipe execution revoke --subject-id UUID
```

- `execution` 子命令只允许在**未携带会话**的隔离 Owner 上下文执行；带会话时直接拒绝，防止自助提权。
- 签发总会生成新 nonce；新 nonce 不解除任何已有的 pending 写。
- `revoke` 只是宿主侧记录（用于 `doctor` 报告与文档提示），不是密码学撤销；真正的撤销是删除私钥/轮转信任根。

### 6.5 写回执（journal）

每个写意图落一个文件：`.ghpipe/state/journal/<intent-digest>.json`

```json
{
  "state": "pending",
  "intent": {"capability": "pr.create", "operation": "pr.create", "payload_digest": "…"},
  "binding": {"repository": "OWNER/REPO", "issue": 42, "branch": "ghpipe/issue-42", "pr": 0},
  "subject_id": "uuid",
  "credential_role": "developer",
  "created_at": "…",
  "result": null
}
```

状态机与规则：

| 迁移 | 触发条件 |
|---|---|
| `→ pending` | 写意图落盘后、发起 HTTP 之前 |
| `pending → refused` | HTTP 400/401/403/404/405/409/422（明确拒绝，证明未发生） |
| `pending → confirmed` | 响应成功且写后回读一致 |
| `pending → reconciled` | 原主体用 `journal resolve` 正向证据回读一致 |
| 保持 `pending` | 网络错误、超时、5xx、响应损坏、回读不一致、进程被杀 |

不变量：

1. 同一「任务 + 操作类别」已有 `pending` 时，新的写意图被拒绝（跨 nonce、跨主体、跨参数拼写都成立）；报错文案必须指向 `journal` 而不是建议重试。
2. 写意图的 payload 参与 digest 计算，改参数不能换来一次新写。
3. `journal list` 列出本机所有未决写；`journal show --id` 只读；`journal resolve --id` 需要原 subject、原任务、正向远端证据三者同时成立。
4. 保密响应（token 类）不落盘正文，只记录 `state: confirmed_secret`。
5. 不存在 `pending → 删除` 的路径；只有 `refused` 会清除 pending。

### 6.6 角色隔离与反越权

目标是：**开发与验收必须是彼此独立的 agent，任何角色都不能借用、换取、伪造另一个角色的身份或凭据。** 下表把「CLI 能强制的」与「只能靠宿主隔离的」分开写，避免把流程约定当成安全机制。

#### 6.6.1 CLI 强制（有测试）

| 规则 | 实现方式 |
|---|---|
| 一主体一角色 | `owner/subjects/<subject_id>.json` 记录 `{subject_id, public_key_fingerprint, work_role, enrolled_at, revoked_at}`；`execution enroll/issue` 对同一个 `subject_id` 或同一公钥指纹签发第二个角色时**直接拒绝** |
| 一角色一密钥 | `doctor` 与任何带会话的命令都会解析 `--subject-key` 的文件指纹；同一密钥文件被登记给两个角色，或同一进程同时持有两个角色的密钥路径时拒绝执行 |
| 一会话一角色 | 会话结构里的 `work_role` + `credential_role` 必须来自固定映射，`capabilities` 必须是该角色白名单的非空子集（§6.2/§6.3） |
| 凭据不可互换 | 配置校验：`apps.developer.credential_ref != apps.delivery.credential_ref`，且两个 App（或账号）必须不同；凭据注册表里同一 `credential_ref` 被两个角色引用时拒绝启动 |
| 凭据身份核对 | 每次取用角色凭据后先核对身份：`GET /installation/repositories` 恰好 1 个仓库 = 配置仓库，`viewer.login == <slug>[bot]`；App 模式还核对 `app_id` 与 `installation_id` |
| 目标绑定 | 仓库、root realpath、cwd、origin、Issue、分支、PR、SHA 全部与会话比对（§6.2、§6.3），跨任务或换 head 一律拒绝 |
| 验收独立性 | 合并前必须存在「与所有开发主体都不同」的 reviewer 主体的、绑定当前 SHA 的批准；`subject_id` 同时出现在开发署名与验收署名时拒绝 |
| 只认注册主体 | reviewer/orchestrator 的会签身份必须来自 `owner/subjects` 注册表且 `work_role` 匹配；自写 identity 文件不能授予角色（identity 只声明） |
| 反提权入口 | `execution *` 只能在**未携带会话**的 Owner 上下文执行；任何角色命令不得写 `owner/`（私钥、信任、主体注册表）与 `credentials.json` |
| 写入不可互换 | journal 记录 `subject_id` + `capability`；同一任务的 pending 写跨主体、跨 nonce 都阻塞（§6.5） |
| Owner 材料可读性告警 | 非 owner 角色的进程若能读到 `owner/keys/*.pem`（权限位允许），`doctor` 报 `isolation: owner_material_readable` 并给出警告——这是宿主问题，不是 CLI 能修的 |

#### 6.6.2 部署前提：单机单用户（不采用容器/独立 OS 用户）

**已确认的部署形态：所有角色在同一台机器、同一个 OS 用户下运行，不使用独立容器或独立 OS 用户。** 一条硬事实：**任何以该用户身份运行的进程，都能读到该用户可读的一切文件**——包括别的角色的密钥、会话、token 文件，也能替换 `ghpipe` 二进制或绕过它直接调 GitHub API / 直接 `git`。

因此安全目标只表述为三件**确实做得到**的事，不做加密、不做生物识别、不加复杂度（D25）：

| 目标 | 做法 | 强度 |
|---|---|---|
| **阻止误用与"顺手提权"** | 会话绑定角色与目标；跨角色命令、跨任务命令、越权能力一律拒绝（§6.6.1） | 强制（CLI） |
| **缩小泄露窗口** | Developer token 按需续签（1 小时，单仓库）；Delivery token 默认 `--ephemeral` **不落盘**；会话短时且写操作单次 | 降低影响，不是阻止读取 |
| **让越权可发现** | 签名身份块 + journal（subject/capability）+ `execution log` + `doctor` 报告长期在盘的凭据 | 可追溯 |

另有一条不依赖本地防护的兜底：**GitHub 侧权限天花板**——即使 Developer 凭据被读走，也推不进主分支、写不了 Issue。

#### 6.6.2.1 安全承诺与边界（必须对用户讲清）

| 能承诺 | 不能承诺 |
|---|---|
| 用 A 角色的会话执行 B 角色的能力，会被拒绝 | 阻止同用户进程**读取**磁盘上的凭据文件 |
| 开发主体不能充当验收主体，批准必须来自 Delivery App 身份且绑定 SHA | 阻止拿到凭据的进程**冒用**该角色 |
| Delivery token 默认不落盘；会话短时且写操作单次 | 阻止攻击者替换 `ghpipe` 二进制或直接调用 GitHub API（同用户进程都有的能力） |
| 所有跨界使用都会在 GitHub 与本地 journal 留下可查痕迹 | 阻止用户自己关闭任何本地检查 |

**不做的事（D25）**：不加密密钥、不接生物识别、不做硬件密钥。理由：同 OS 同用户下这些只是提高门槛而非形成边界，复杂度与收益不成比例。凭据保护回到最朴素的做法——**文件权限（POSIX 0600/0700，Windows 用户配置目录 ACL）+ 尽量短的有效期 + 尽量少落盘 + 全程可审计**。

#### 6.6.3 每个角色只能看见自己那条路

- `doctor --for develop|review` 只读本角色凭据，输出 `isolation: {role, subject_id, key_path, shared_with: []}`；`shared_with` 非空即报 fail。
- `status` 的 `role` 字段、`Review` 的身份块、`cleanup` 审计声明、journal 记录都带 `subject_id`，因此任何"同一主体既开发又验收"的行为都会在账本上留下可直接检测的痕迹。
- 交接（handoff）只能换**主体**，不能换角色：`task handoff` 替换的是 Developer 主体，Reviewer 与 Orchestrator 永远由 Owner 另行签发。

#### 6.6.4 Token 安全：威胁模型与对策

威胁假设：**Agent 可能被提示注入或自身出错，主动尝试读取、借用、换取别的角色的凭据**（读 `~/.config/ghpipe/owner/keys/*.pem`、让 CLI 给它签发另一个角色的 token、用开发凭据冒充验收）。对策按"真正的边界 → 让偷到也没用 → 能发现"三层排列：

| 威胁 | 对策 | 性质 |
|---|---|---|
| 用开发凭据直接 `git push` 到默认分支 | **GitHub ruleset**：默认分支的 update 规则只给 Delivery App `pull_request` 绕过，其他任何 actor 直接推送被 GitHub 拒绝 | 强制（GitHub 侧） |
| 用开发凭据提交"批准"以绕过独立验收 | 合并门禁要求批准必须来自**配置的 Delivery App 机器人身份**（`user.login == <delivery slug>[bot]`）且带绑定当前 SHA 的 reviewer 身份块；Developer App 的批准一律不计入 | 强制（CLI 侧判定 + GitHub 侧身份） |
| 让 CLI 用开发会话签发 delivery token | 会话绑定 `credential_role`；`auth token` 只能续签该角色凭据，且要求 `token` 能力；跨角色请求直接拒绝 | 强制（CLI） |
| 读别的角色的 token/私钥文件 | 每台宿主只放本角色材料；`doctor` 检测到非 Owner 角色可读 `owner/keys/*`、或两个角色共用同一密钥/凭据文件时，报 `isolation` fail；`OpenValidated` 用 `O_NOFOLLOW` + `SameFile` 防符号链接掉包，凭据文件拒绝多链接 | 检测 + 加固（真正的隔离必须靠宿主） |
| 偷到 installation token 后横向移动 | token 1 小时过期、只对单个仓库、按角色最小权限（developer 无 Issues 写、无 Workflows/Administration；delivery 无绕过规则能力）；GitHub 侧仍是最后一道 | 强制（签发时收窄 + 服务器裁决） |
| 偷到会话文件后 replay 写操作 | nonce + journal 单次写；已完成的写不可重放；存在 pending 时任何新写都被拒绝；会话 ≤24h 且绑定 root/分支/PR/SHA | 强制（CLI + 账本） |
| 偷到 Owner 签名私钥后自签任意会话 | Owner 私钥**只存在 Owner 管理上下文**，不下发到任何 agent 宿主；`execution *` 只能在无会话的 Owner 上下文执行；`doctor` 检测到 agent 进程可读 Owner 材料即告警 | 宿主隔离 + 检测（CLI 无法阻止同权限进程） |
| Agent 通过读环境变量或子进程拿到 token | `run` 执行项目命令时清洗 `GH_*`/`GITHUB_*`/所有 `GHPIPE_*` 与 `execution.strip_env`；CLI 从不打印 token，`--json` 不含秘密，错误信息只含脱敏路由；token 不进 argv（git 用内置凭据助手） | 强制（CLI） |
| 提示注入诱导 Agent 去读别的角色材料 | Skills 明确禁止读取/搬运其他角色凭据；`journal`/`execution log` 留下签发与使用痕迹 | 约定 + 审计（不是沙箱） |

**必须如实说明的边界**：同一 OS 用户、同一容器里，任何程序都能读同用户可读的文件、改工具二进制、绕过 `ghpipe` 直接调 GitHub API 或直接 `git`。所以生产环境的要求是：

1. 已确认不采用容器/独立 OS 用户，因此**不能用"文件权限"作为角色隔离手段**（同用户进程一律可读）。防线是 §6.6.2 的三条：阻止误用与顺手提权、缩小泄露窗口、让越权可发现。
2. 凭据**不做加密、不接生物识别**（D25）：同用户下那只是提高门槛而不是边界。凭据的"少落盘"（Delivery token 默认 `--ephemeral`）与"短有效期"（token 1 小时、会话按任务）才是实际收益。
3. 若将来愿意改变部署形态，容器/独立用户仍是更强方案，但不是本设计的前提；`doctor` 会把"当前是单用户模式、哪些凭据长期在盘上"如实报告出来，不夸大保护强度。

#### 6.7 单机模式的凭据存放与最小驻留

凭据一律是**普通文件**（不做加密、不做生物识别，D25），保护来自"放得少、活得短、可审计"：

| 凭据 | 存放 | 有效期 | 说明 |
|---|---|---|---|
| Developer App installation token | 文件 0600（或宿主注入的环境变量） | 1 小时，按需续签 | 高频使用；GitHub 权限天花板限制爆炸半径 |
| Delivery App installation token | **默认不落盘**（`--ephemeral`） | 单次命令 | 验收/合并/清理时按需签发、用完即弃 |
| 会话 JSON | 文件 0600 | ≤24h（Reviewer/Orchestrator 按任务，通常 1–2h） | 绑定角色/仓库/root/分支/PR/SHA；写操作单次 |
| 主体私钥 | 文件 0600（父目录 0700；Windows 用户配置目录） | 长期 | 用于证明持有会话对应的主体 |
| App 私钥 PEM | 文件 0600 | 长期 | 换取 installation token |
| Owner 签名密钥 | 文件 0600，只在 Owner 上下文 | 长期 | 签发会话与轮换信任 |

```text
ghpipe execution keygen --output /protected/key.pem   # 生成主体/Owner 密钥，只输出公钥
ghpipe execution issue  ...                           # 签发会话；只能在无会话的 Owner 上下文执行
ghpipe auth token [--status] [--ephemeral]            # 按需换 token；--ephemeral 不写盘
ghpipe execution log                                  # 谁在何时签发了哪个角色、绑定什么
ghpipe doctor --for handoff                           # 报告 isolation 与"哪些凭据长期在盘上"
```

`doctor` 把真实情况直接摆出来，不宣称比实际更强：

```json
"isolation": {"mode": "same_user",
              "persistent_secrets": ["developer.token", "subject keys", "app pem", "owner signing key"],
              "ephemeral": ["delivery.token", "sessions"],
              "notes": ["同用户进程可读上述文件；本工具不做加密与生物识别（D25）"]}
```

#### 6.8 宿主能力判定：主会话如何确定"能不能派发独立子 agent"

主会话（Orchestrator）**必须**用宿主工具的**原生子 agent** 派发 Developer 与 Reviewer——各自拥有独立上下文的独立 agent，而不是主会话在同一上下文里"扮演两个角色"。这条要求必须由**可执行判定**支撑，不能靠"听说支持/不支持"。

##### 6.8.1 主流工具的能力形态（先看别人怎么做）

| 工具 | 官方能力 | 关键机制 |
|---|---|---|
| Codex | **Subagent workflows 默认启用**（ChatGPT Work / Codex CLI / IDE）；本地 Codex 还支持用配置定义 custom agents | 主会话派发 → 子 agent 并行工作 → 结果汇集到一个回复；子 agent 的活动可见 |
| Claude Code | 三层：**subagents**（同一会话内、独立上下文与工具集）、**background agents**（多个独立会话并行）、**cross-session messaging / agent teams**（会话间传递消息、团队编排） | 自定义 subagent 定义在 `.claude/agents/*.md`，靠 `description` 触发委派；可限制子 agent 的工具集（例如只读） |
| OpenCode | **primary agents + subagents**：subagent 由主 agent 调用，或用户 `@` 显式点名；内置 General/Explore/Scout | 子 agent 有独立上下文与工具权限；可在配置里自定义 |

结论：主流工具的能力不是"有/没有"的二元，而是**阶梯**。因此判定要回答的是"当前阶段需要哪一级、当前工具处在哪一级"。

##### 6.8.2 能力阶梯与最低要求

| 级别 | 形态 | 能否满足"独立验收" |
|---|---|---|
| L0 | 无委派能力（只有一个上下文） | 否 |
| L1 | 同会话内子 agent（独立上下文、返回结果） | **可以**，需通过 §6.8.3 的三项属性验证 |
| L2 | 独立会话/进程（如 background agents、独立 CLI 进程） | 可以（评测：结果收集可能要走文件/账本） |
| L3 | 会话间消息 / 团队编排（可监督多个会话） | 可以，且更适合连续编排 |

交付类操作的最低要求是 **L1 且通过三项属性**：

| 属性 | 含义 | 不满足时的后果 |
|---|---|---|
| A1 任务投递 | 子 agent 能收到任务正文与角色边界 | 子 agent 只能自己猜任务——这正是本仓库自举时踩到的坑 |
| A2 上下文独立 | 子 agent 看不到父会话内容，子 agent 之间互不可见 | "独立验收"名存实亡（等于自审自过） |
| A3 结果可收集 | 主会话能等待并拿到结果 | 无法判定通过/返修，流程无法继续 |

另有两条**加分项**（不满足不阻断，但要在账本里标注）：A4 子 agent 有独立主体身份（对应 CLI 会话隔离）；A5 能限制子 agent 的工具集（例如 Reviewer 禁写）。

##### 6.8.3 判定方法：声明 + 探针 + 证据

**第一步 声明**（先验，不作证据）：`ghpipe doctor --for handoff` 读取宿主工具的自报能力（配置项 `host_tool: {name, version, subagents: true|false, agent_definitions: path?}`），只用来选择探针的派发方式，不用来下结论。

**第二步 探针**（唯一证据）。主会话生成三个随机 nonce：`N_A`、`N_B`、`N_X`，只把 `N_A` 给 A、`N_B` 给 B，`N_X` 谁也不给；要求两个子 agent 各回报一行 `PROBE <自己的 nonce>`，并额外回答"你在自己的上下文里还能看到哪些 nonce"。判据：

1. A 回报 `N_A`、B 回报 `N_B` → A1 成立；
2. A 报不出 `N_B`/`N_X`、B 报不出 `N_A`/`N_X` → A2 成立；
3. 主会话在超时内收到两份结果 → A3 成立。

三者全过才判 `host_tool: compatible (level=L1|L2|L3)`。

**第三步 失败分类**（把"失败"拆成可处置的类别，而不是笼统的"不支持"）：

| 分类 | 现象 | 处置 |
|---|---|---|
| `no_dispatch` | 工具根本没有派发能力 | 判 L0；按 §6.8.4 走降级阶梯 |
| `no_task_delivery` | 子 agent 起来了，但没拿到任务（本仓库自举时实测到的情况） | 换派发方式：①把任务写进子 agent 会读的文件/Issue（推荐，见 §6.8.5）；②改用工具自己的 agent 定义（如自定义 subagent 文件、`@` 点名）；③显式打开"继承父上下文"的派发选项 |
| `shared_context` | 子 agent 能看到父会话或彼此的内容 | 不满足 A2：只能用于辅助工作，**不得用于验收**；换 L2/L3 或用工具的分隔选项 |
| `no_collect` | 只能后台跑、父会话拿不到结果 | 结果走文件/账本回收（每次都留下证据），并在状态里标注 `collection: via_ledger` |
| `restricted_absent` | 无法限制 Reviewer 的写权限（A5 不满足） | CLI 侧补偿（Reviewer 会话本身没有 `commit`/`push` 能力），并在账本标注 |

**第四步 记录**：探针结果写 `.ghpipe/state/host-probe.json`（`tool`、`version`、`level`、`A1..A5`、`failures[]`、`nonce_hashes`、`observed_at`），并在当前 Issue/PR 留一条摘要评论作为账本证据。`doctor --for handoff` 的状态机：无记录 → `host_tool: unprobed`（交付类命令拒绝执行）；记录存在且 A1–A3 全过 → `compatible`；存在分类失败 → `incompatible` + 分类原因。

##### 6.8.4 降级阶梯（每一级都要用户显式批准并留痕）

| 级别 | 形态 | 独立性标注 | 允许的用途 |
|---|---|---|---|
| 默认 | 原生子 agent（L1–L3） | `native_subagent` | 开发与验收 |
| 降级 1 | 独立会话/进程（background agent、独立 CLI 进程） | `separate_session` | 开发与验收（结果走文件/账本收集） |
| 降级 2 | 人工派发（用户自己开两个会话、两把凭据） | `manual_dispatch` | 开发与验收 |
| **禁止** | 主会话自己开发、自己验收 | `self_review` | **任何情况下都不允许** |

降级必须写进 `execution log` 与该 Issue/PR 的交接评论；`doctor` 在降级模式下显式打印 `independence: separate_session|manual_dispatch`，不把它说成原生子 agent。

##### 6.8.5 派发通道：任务正文放哪里

主流工具的委派都靠"把指令交给子 agent"完成，而**不同工具的投递方式不同**：Codex 靠派发时的任务文本（可用自定义 agent 指令），Claude Code 靠 subagent 定义文件 + `description` 触发，OpenCode 靠配置 + `@` 点名。因此 ghpipe 的 Skill 按下面顺序选通道：

1. **工具原生派发**（首选）：用宿主提供的方式把任务交给子 agent；
2. **任务文件通道**（兜底）：把任务正文写到工作区的约定文件（本项目自举阶段用 `docs/TASK.md`），派发指令里要求子 agent **先读该文件**；文件内容与 Issue 正文一致，来源仍是账本；
3. **账本通道**：任务范围、变更要求、验收标准始终在 Issue/PR 留一份（子 agent 若能联网就自行读取；不能联网时由调度者把它落到工作区文件）。

本项目自举阶段实测：某工具的派发消息没有进入子 agent 上下文（子 agent 明确回报"没有任务正文"），改用任务文件通道后子 agent 即可正常开工——这就是"失败分类 + 通道选择"要解决的问题，而不是判"工具不支持"。

---

## 7. GitHub 层

### 7.1 传输

```go
type Client struct {
    http    *http.Client      // Timeout: 60s（单次请求）
    base    string            // https://api.github.com
    token   string            // installation token 或 App JWT
    scheme  string            // "Bearer"
}
```

- 固定头：`Accept: application/vnd.github+json`、`X-GitHub-Api-Version: 2022-11-28`、`Cache-Control: no-cache`、受控 `User-Agent`。
- 不接受 `--repo`/`--hostname` 形式的仓库覆盖；仓库只来自配置与签名。
- App JWT 走 `Authorization: Bearer <jwt>`（gh 的默认 token 方案对 App JWT 不适用，这是现实现踩过的坑，直接固化为默认行为）。
- 不重试、不缓存、不落盘响应正文。

### 7.2 REST 分页契约

「取完所有页」不等于「结果完整」。完整性契约必须逐条实现并有测试：

| 检查 | 失败处理 |
|---|---|
| 只跟随 `Link: rel="next"`；短页有 next 继续，满页无 next 即结束 | 视为不完整 |
| 去除调用方传入的 `page`/`per_page`，强制 `per_page=100` | — |
| 页指纹去重（`sha256` 规范化 JSON） | 出现重复页 → 结果不完整 |
| 提供 `total_count` 的端点必须满足 `len(items) >= total_count` | 不满足 → 结果不完整 |
| 最大页数预算（默认 50 页，可按命令收紧） | 超预算 → 结果不完整 |
| 单次请求超时 60s，整体有上下文超时 | 超时 → 结果未知 |

任何不完整结果都不得用于写入决策（可以用于只读报告并标注 unknown）。

### 7.3 GraphQL 契约

- 每次查询只允许一个分页 connection，统一使用 `$endCursor`。
- 逐页校验：`pageInfo.hasNextPage` 与「是否还有下一页」一致；`endCursor` 非空且不重复；`totalCount`（若返回）与节点数一致。
- 业务校验：`statusCheckRollup` 的 `oid` 必须等于请求 SHA；`CheckRun.checkSuite.commit.oid` 必须等于请求 SHA；cross-reference 只认同仓库的 `PullRequest`。
- `errors` 非空即失败，不返回部分数据。

使用 GraphQL 的地方仅三处：Issue 的 `timelineItems(CROSS_REFERENCED_EVENT)`、commit 的 `statusCheckRollup`、`viewer{login}` 身份核对。其余全部 REST。

### 7.4 错误分类与脱敏

```go
type ErrorKind int
const (
    KindHTTP ErrorKind = iota  // 明确 HTTP 状态
    KindDNS
    KindTLS
    KindProxy
    KindTimeout
    KindRefused
    KindReset
    KindUnreachable
    KindAccessDenied
    KindDecode                // 响应不是预期 JSON
    KindIncomplete            // 分页/游标/总数不完整
    KindContract              // 响应结构不符合契约（缺字段、SHA 不匹配）
    KindProcess               // 本地 git 进程失败
)
```

- 分类来自 Go 错误类型（`net.DNSError`、`x509.UnknownAuthorityError`、`*url.Error.Timeout()` 等），不再依赖 stderr 文本正则。
- 错误信息只包含：方法、**脱敏后的端点模板**（形如 `/repos/{value}/{value}/pulls/{value}`）、HTTP 状态、错误类别。不含 token、仓库名、原始 URL、响应正文、环境变量。
- 401 归类为 `authentication`；403/404 归类为 `repository_access_or_visibility`（不凭 404 判断「无权限」还是「不存在」）。

### 7.5 端点清单（实现范围）

| 用途 | 端点 |
|---|---|
| 安装与身份 | `GET /installation/repositories`、`GET /app`、`GET /repos/{repo}/installation`、`POST /app/installations/{id}/access_tokens`、`POST /app-manifests/{code}/conversions`、`GET /user`、`GET /users/{login}` |
| 仓库 | `GET /repos/{repo}`、`GET/PATCH /repos/{repo}/branches/{branch}`、`GET /repos/{repo}/compare/{base}...{head}` |
| Issue/PR | `GET/POST/PATCH /repos/{repo}/issues[...]`、`GET/POST /repos/{repo}/issues/{n}/comments`、`GET/POST/PATCH /repos/{repo}/pulls[...]`、`GET/POST /repos/{repo}/pulls/{n}/reviews`、`GET /repos/{repo}/pulls/{n}/commits`、`PUT /repos/{repo}/pulls/{n}/merge` |
| 标签/里程碑 | `GET/POST /repos/{repo}/labels`、`POST/DELETE /repos/{repo}/issues/{n}/labels[/{name}]`、`GET/POST/PATCH /repos/{repo}/milestones` |
| 检查 | `GET /repos/{repo}/commits/{sha}/check-runs`、GraphQL `statusCheckRollup` |
| 规则 | `GET /repos/{repo}/rules/branches/{branch}`、`GET /repos/{repo}/rulesets[/{id}]`、`GET /orgs/{org}/rulesets[/{id}]`、`GET /repos/{repo}/branches/{branch}/protection` |
| Git 引用 | `GET /repos/{repo}/git/ref/heads/{branch}`、`GET/POST /repos/{repo}/git/refs`、`GET /repos/{repo}/git/matching-refs/heads/{pattern}` |
| Actions | `GET /repos/{repo}/actions/runs/{id}` |
| 内容 | `GET /repos/{repo}/contents/{path}?ref=…`（设计文件存在性、默认分支配置核对） |

### 7.6 gh CLI、go-gh 与自研传输的取舍

| 维度 | 继续复用 `gh` 子进程 | 引入 go-gh（v2） | 自研传输（本方案） |
|---|---|---|---|
| 安装依赖 | 需 `gh ≥ 2.48`（`--slurp`） | 无进程依赖，但引入第三方库 | 无 |
| 命令面可得性 | 全部可得 | **不可得**：`gh pr create` 等实现在 `cli/cli` 的 `internal/` 包，外部不可导入 | 需要什么写什么 |
| 认证模型 | 依赖 gh 的 token 优先级（可能回落到个人登录） | 提供 gh 的 hosts.yml / keyring / 环境变量发现 | 只用自己的 installation token；不存在「回落到个人登录」的路径 |
| 分页完整性 | gh 负责取页，项目仍需逐页校验 | 同上，helper 会把页拍平，证据丢失 | 逐页校验就是实现本身 |
| 错误分类 | 只能解析 stderr 文本 | 取决于库的包装 | Go 错误类型直接分类 |
| 可测试性 | 打桩 `subprocess` | 可注入 `http.Client` | 可注入 `RoundTripper` |
| 进程开销 | 每次调用一个进程；一次完整分页一个进程 | 无 | 无 |
| 结论 | 基线，作为演进起点 | 收益有限、且会引入**不想要**的凭据发现层 | **采用** |

补充判断依据：现实现里绝大多数「gh 调用」其实已经是 `gh api`（纯 REST），包括所有带身份的写入；真正依赖 gh 特有语义的只有 `pr view --json reviewDecision,mergeStateStatus`（两条字段，可用 GraphQL 或 REST 直接取）、`repo create/clone/edit`、`issue list/create`（REST 直取）和 `git credential` 助手（本方案用内置凭据助手替代）。因此去掉 gh 的代价很小，收益（无依赖、准确错误分类、可注入测试）很大。

`gh` 对**人**仍然可用（例如 `gh pr checks` 供人工查看），但 ghpipe 的任何门禁判定都不读 gh 的输出。

---

## 8. Git 层

### 8.1 封装

`internal/gitx` 只暴露语义化方法，不暴露裸命令执行（除测试用 `Runner`）：

```go
CurrentBranch(dir) (string, error)          // 空 = detached
OriginURL(dir) (string, error)
IsShallow(dir) (bool, error)
ResolveSHA(dir, ref) (string, error)
IsAncestor(dir, a, b) (bool, error)
Status(dir) ([]StatusEntry, error)
Worktrees(dir) ([]Worktree, error)
CommitTree(dir, message, env) (string, error)   // -F - 走 stdin
PushExact(dir, sha, branch, env) error         // <sha>:refs/heads/<branch>
FetchSHA(dir, sha, env) error
DeleteBranchGuarded(dir, branch, sha, env) error
```

全部命令用 argv 数组执行，不使用 shell；输出解析失败一律报错，不做模糊兜底。

### 8.2 分支守卫

```go
func RequireDevelopmentBranch(root string, cfg Config, target *string, remoteDefault *string) (string, error)
```

拒绝：`main`、`master`、`config.default_branch`、`origin/HEAD` 指向分支、`remoteDefault`（来自远端实时读取）、detached HEAD；`target` 必须等于当前分支。只读 Review、测试、状态检查、已授权合并/快进/收尾允许在默认分支或 detached 下执行。

「一个 Issue 一个开发分支、禁止在主分支开发」不是一个开关，而是**三层同时生效**：

| 层 | 机制 | 阻断了什么 |
|---|---|---|
| **GitHub 侧规则（真边界）** | 默认分支 ruleset：`pull_request`（≥1 批准、旧批准失效、最后推送批准）+ `required_status_checks`（严格、来源绑定）+ `non_fast_forward` + `deletion`，`bypass_actors = []`；再有一条 writer ruleset：默认分支仅 `update`，bypass 只给 Delivery App（`pull_request` 模式）。特性分支命名空间 `refs/heads/ghpipe/issue-*` 的 `creation`+`update` 只给 Developer App（`always`）；仓库开启 `delete_branch_on_merge` | 任何角色（包括偷到 Developer token 的攻击者）都无法直接推送默认分支；非 PR 的改动进不了主分支 |
| **CLI 侧守卫（快速反馈）** | 会话绑定 `branch == ghpipe/issue-<issue>`；`commit`/`push`/`pr.create` 前校验：当前分支 = 目标分支 = 已签分支，且不是默认分支/`origin/HEAD`/detached；push 只接受 `<sha>:refs/heads/<已签分支>`（不带 force、不推 tag）；`pr.create` 的 `head` 必须是当前分支、`base` 必须等于**实时读取**的默认分支；PR 正文必须含唯一 `Closes #<已签 Issue>` | 误操作在本地就被拦下，不必等 GitHub 拒绝；也避免"在 main 上改完再挪分支"这类事故 |
| **账本检测（事后可查）** | `attribution.required` 时 push 前核验未发布提交的署名 trailers，手工拼的提交推不上去；`status`/`doctor` 报告"本地在默认分支且有未提交改动"、"远端默认分支出现非 Delivery App 合并提交"、"存在不属于任何任务的 `ghpipe/issue-*` 分支" | 绕过工具直接操作留下的痕迹可被发现并追责 |

三层的分工是刻意的：**真正的强制只有 GitHub 规则**（本地进程无法阻止同权限程序改文件或直接调用 API），CLI 守卫负责让错误尽早暴露，账本负责事后可查。`ghpipe init checks` 安装这三条规则并**回读校验**；`doctor --for handoff` 每次核对规则是否仍然生效（被改动就报 fail）。

### 8.3 署名提交

- 只注入本次子进程的 `GIT_AUTHOR_*` / `GIT_COMMITTER_*`，不写全局或仓库配置，不自动 `git add`。
- 提交信息头部必须由调用方提供（`--message-file`），且不得包含生成的署名块或 `Co-authored-by`/`Aipipe-*` 行；trailers 由工具追加：

```text
Ghpipe-Agent: <subject-uuid>
Ghpipe-Tool: <tool>
Ghpipe-Model: <model>
Ghpipe-Role: developer
Ghpipe-Credential-Role: developer
Ghpipe-Issue: OWNER/REPO#42
```

- 提交后回读 `%an %ae %cn %ce %B`，与期望完全一致，否则报「可能被 hook 改写」并拒绝继续 push。
- 处于 `MERGE_HEAD`/`CHERRY_PICK_HEAD`/`REVERT_HEAD`/`rebase-merge`/`rebase-apply` 时拒绝执行普通署名提交。

### 8.4 推送与内置凭据助手

推送不再依赖 `gh auth git-credential`：

```text
git -c credential.helper= \
    -c credential.helper='!ghpipe git-credential' \
    push origin <sha>:refs/heads/<branch>
```

父进程通过环境变量把「角色 + 会话/主体密钥路径」传给助手；助手指令只读注册表并输出 `username=x-access-token` 与 `password=<token>`，token 不出现在命令行、不出现在 `ps` 输出。

推送前检查：origin 必须是 `https://github.com/...`；远端默认分支实际 SHA 必须读得到（`ls-remote`）；分支名与 SHA 符合格式；`attribution.required` 时核验未发布提交的署名完整性。

### 8.5 清理（cleanup）

保持现实现的保护语义并显式化：

1. PR 必须已确认合并；head/base 必须同仓库；分支不能是默认分支。
2. 远端分支必须已不存在（用 `ls-remote --heads` 广告证明，不用 404 推断）。
3. 本地分支必须存在且 SHA 等于合并 head；工作区必须干净；分支不能被其他 worktree 占用。
4. 目标分支必须先 fetch，并确认 merge commit 是其祖先。
5. 删除本地分支时先把临时 upstream 指向「精确 PR head」建立的锚点 ref，用 `git branch -d` 的原生安全检查；失败时恢复被改动的 `branch.<name>.*` 配置并删除锚点。
6. 成功后在原 PR 发布**绑定 repo/PR/head/merge/checkout_scope 的 Delivery App 审计声明**；声明只表示该 checkout 范围已清理，不表示所有主机完成。

远端与本地由不同主体负责，这样"远端删掉"不需要任何 agent 在跑：

| 对象 | 谁删 | 依据 |
|---|---|---|
| 远端开发分支 | **GitHub 自己**（`delete_branch_on_merge`，`init checks` 强制开启并回读） | 合并即触发，不依赖 agent 存活 |
| 本机开发分支与 tracking ref | CLI `cleanup`（受签 Orchestrator） | 上面的 1–5 条保护；SHA 与合并 head 必须一致 |
| 审计声明 | CLI `cleanup` 成功后由 Delivery App 发布到原 PR | scope = 该 checkout；只声明本机范围 |

`done` 因此要求三项互相独立的证据同时成立：**远端不存在**（`ls-remote` 广告证明）、**本机不存在**（local 与 tracking ref 都没有）、**审计声明存在且 scope 匹配**。任一缺失都停在 `closing`——"远端看不到了"不等于本机清理完成，也不等于所有主机完成。

---

## 9. 命令面

### 9.1 通用约定

全局参数：`--project DIR`、`--config FILE`、`--credentials FILE`、`--session FILE`、`--subject-key FILE`、`--json`、`--no-color`、`--verbose`。

- `--json` 时 stdout 只输出一个 JSON 信封；人类输出走默认格式；诊断信息一律走 stderr。
- 会话参数可出现在子命令之后（保持「全局选项位置自由」），解析器需支持这一形态。
- 退出码：

| 码 | 含义 |
|---|---|
| 0 | 确定成功 / 状态 ready |
| 1 | 确定失败（未发生副作用，或 GitHub 明确拒绝） |
| 2 | **结果未知**（可能已写），必须用 `journal` 回读 |
| 3 | 用法、配置、版本或授权前置错误（未联网、未写） |

### 9.2 结果信封

```json
{
  "schema_version": 1,
  "command": "pr.review",
  "status": "succeeded",
  "repository": "OWNER/REPO",
  "target": {"kind": "pr", "number": 7},
  "data": {"head": "<sha>", "review_id": 12345},
  "error": null,
  "next": [],
  "observed_at": "2026-09-13T12:00:00Z"
}
```

- `status ∈ {succeeded, failed, unknown, ready, pending}`；`unknown` 必须同时给出 `journal` 恢复提示。
- `data` 字段按命令定义并写入 `docs/cli.md`（随资源内嵌），字段一旦发布只增不改。

### 9.3 命令树

```text
ghpipe
├── version | --version
├── inspect
├── update        [--check] [--force] [--layout separate|suite]
├── status        (--issue N | --pr N) --role developer|delivery [--offline]
├── doctor        --for plan|publish|develop|review|bootstrap|handoff [--offline|--connectivity] [--check-sha SHA]
├── run           <name>
├── commit        --issue N --message-file FILE [--identity FILE] [--author-identity FILE] [--coauthor-identity FILE]
├── push          <branch> [--identity FILE]
├── pr            create | edit | comment | review | merge
├── issue         view | list | comment | close | reopen
├── api           <METHOD> <path> [--input FILE]
├── task          inspect | next | claim | handoff | release
├── journal       list | show --id ID | resolve --id ID
├── identity      create | show
├── metadata      [--issue N | --pr N] [--apply] | workflow
├── quality       check --base SHA [--head REF] [--run-tests]
├── cleanup       --pr N
├── publish       (--plan FILE --design-ref SHA | --issue-file FILE) [--apply]
│                 （另有 --bug-file FILE：缺陷 Issue，见 §10.7）
├── bug           new --key <id> [--draft]     # 生成缺陷模板（--draft 只写本地草稿，交主会话/Owner 归档）
├── triage        --issue N [--check] [--accept --severity S --milestone M | --reject --reason R] [--apply]
├── report        preview | submit | status     # 工具自身缺陷上报（默认关闭，需专用凭据）
├── verify        regression --pr N [--base SHA] [--test NAME]   # 验证回归测试有效性（仅 Reviewer/Orchestrator）
├── release       --milestone N --plan FILE --design-ref SHA [--apply]
├── config        set | validate
├── auth          status | token | trust-app
├── init          repo | app | checks | metadata | resources | seed
│                 （init app 支持 --role/--existing/--no-browser/--paste，init app repos 调整安装范围）
├── skills        list | new | clone | diff | reset | sync | status
├── execution     keygen | enroll | trust | issue | list | revoke | rotate | log
└── git-credential（内部助手，供 git 调用）
```

### 9.4 与旧命令的映射

| aipipe | ghpipe | 变化 |
|---|---|---|
| `aipipe github -- pr create …` | `ghpipe pr create …` | 去掉 gh 透传与 argv 重组；参数显式 |
| `aipipe api GET/POST path` | `ghpipe api GET PATH` / `ghpipe api POST PATH --input FILE` | 保留为唯一通用 REST 入口，写路径仍受能力白名单 |
| `aipipe receipt …` | `ghpipe journal …` | 状态机化，语义不变 |
| `aipipe --execution-session F --subject-key K` | `ghpipe --session F --subject-key K` | 参数缩短，语义不变 |
| `--result-json` | `--json` | 统一信封 |
| `aipipe init …`（含 `--with-apps/--with-checks`） | `ghpipe init repo\|app\|checks\|metadata\|resources\|seed` | 组件显式，去掉交互式「or」流程 |
| `aipipe auth issue-token` | `ghpipe auth token` | 删除弃用别名 |
| `aipipe scripts/*.py` | — | 删除 |

---

## 10. 业务模块

### 10.1 status（只读交接状态）

`ghpipe status (--issue N | --pr N) --role developer|delivery [--offline] --json`

- 装配顺序：本地配置与兼容性 → 检出信息 → 角色凭据（`--offline` 跳过）→ Issue/PR/Review/Checks/规则事实 → 阶段投影 → 结算事实 → 结束回读 head。
- 阻断项（blockers）带 `code` + `severity ∈ {pending, failed, unknown}`，整体状态取最严重值：`failed > unknown > pending > ready`。
- 关键 code 保留：`pr_missing`、`ambiguous_association`、`task_blocked`、`draft`、`cancelled`、`head_missing`、`head_changed`、`merge_state`、`checks`、`reviews`、`closing_main_ci`、`closing_remote_absent`、`closing_cleanup_report`、`closing_metadata`、`closing_local_cleanup`、`closing_issue`。
- 每个 code 给出 `next_actions`（面向人的可执行建议），done 时 `next_actions` 为空。
- 已合并 PR 的 checks 绑定 `merge_commit_sha`，并单独展示当前默认分支 head；不得用后续 main 绿色掩盖本次合并提交的失败。
- 结束时回读 PR head；两次不一致 → `head_changed`（failed）。

### 10.2 doctor / 连通性诊断

| 用途 | 检查内容 |
|---|---|
| `--for plan` | 仅本地：CLI 兼容性、命令配置合法性 |
| `--for develop` | 分支守卫 + developer 凭据身份 + 仓库可见性（`installation/repositories` 恰好 1 个且等于配置仓库；`viewer.login == slug+"[bot]"`） |
| `--for review` | 同上，用 delivery 凭据 |
| `--for bootstrap` | bootstrap 模式 + `--check-sha` 的真实初始化检查来源 + 完整规则审计 |
| `--for handoff` | 两角色 + 默认分支一致 + squash/删除分支设置 + 实际工作流存在 + 完整规则审计 + strict 配置已合入默认分支 |
| `--offline` | 不读凭据、不联网，远端事实固定 unknown（仅 plan/develop/review 允许） |
| `--connectivity` | 复用 merge 的凭据清理与传输，仅做一次 `GET /repos/{repo}`，不评估门禁、不写任何东西 |

输出必须包含 `business_acceptance: {evaluated: false}` 与「这不是业务验收」的说明。

### 10.3 metadata

事实收集：Issue → 默认分支 → `timelineItems(CROSS_REFERENCED_EVENT)` → 同仓库、base 为默认分支、唯一 `Closes #N` 的 PR → 打开中的 PR 取其 reviews → 分支是否存在。

投影（纯函数）：

| 条件 | Issue 阶段 |
|---|---|
| Issue 关闭且有已合并 PR | 该 PR 的阶段（done / closing） |
| Issue 关闭且无已合并 PR | cancelled |
| 有活动 PR | `pr_stage(pr)`：draft→active；有 CHANGES_REQUESTED→changes-requested；否则 review |
| 无活动 PR 但有已合并 PR | closing |
| 无 PR 且分支存在 | active |
| 无 PR 且无分支 | ready |

同步：期望标签集合与现状求差 → 只增删受管标签（`ghpipe:*`）→ PR 继承 Issue 的非阶段标签、milestone 置空 → 若同一对象出现多个活动 PR 则拒绝同步 → 写后重新读取事实并重算，若期间生命周期变化则报错不重试。重复 DELETE 404 只有在完整分页回读确认标签确实不存在时才视为成功。

workflow 事件路由（`ghpipe metadata workflow`，读 `GITHUB_EVENT_*`）：

| 事件 | 目标 |
|---|---|
| `pull_request_target` | 该 PR 关联的 Issue（跨仓库/非默认 base 跳过） |
| `workflow_run` | 仅接受名为 `ghpipe review signal`、事件为 `pull_request_review`、conclusion 成功、唯一 PR 关联的运行 |
| `pull_request_review` | 只做信号，不写（默认分支源码不可在此事件下执行可信写入） |
| `issues` | 该 Issue（若带受管标签） |
| `create`/`delete` | `ghpipe/issue-N` 分支事件 |
| `schedule` | 有界扫描：打开 Issue 最多 10 页 + 最近 14 天关闭 Issue 最多 2 页 |
| `workflow_dispatch` | `closed_scope=recent|history`（history 最多 10 页） |

扫描截断、重复页、响应非法都要作为独立失败输出，但已发现的任务继续逐项同步；最后以非零退出报告。

### 10.4 checks 与 policy（合并门禁）

- `checks.Required(cfg)`：取 `ci.required_checks`，校验 context 唯一、integration_id 为正。
- `checks.Evaluate(client, repo, sha, branch, cfg)`：读有效规则 + commit rollup；同名 check-run 与 commit status 同时存在时必须都成功；`skipped`/`neutral` 归入可接受 conclusion 但保留原始 conclusion；来源不匹配（wrong_source）为 failed；规则不可读为 unknown。
- `policy.Audit(owner)`：审计有效规则与 legacy protection，要求「质量规则无 bypass（`bypass_actors == []`，`current_user_can_bypass == never`）+ 至少 1 批准 + 旧批准失效 + 最后推送批准 + 严格来源必需检查 + 禁强推/禁删除」；main writer 规则只给 Delivery App `pull_request` 资格；feature writer 规则覆盖整个 `ghpipe/issue-*` 命名空间且只给 Developer App。
- `policy.MergeGate(...)`：实现 §2.4 的顺序判定；返回 `{basis: native_clean|verified_pr_only_writer, eligible_to_attempt, confirmed_mergeable}`，并在 result 中透传。

### 10.5 quality（质量门禁）

`ghpipe quality check --base SHA --head REF [--run-tests]`

- 比较 base 的已生效策略与 base/head 完整文件树：head 不能把 strict 降级为 bootstrap；bootstrap 只在「base 也是 bootstrap 且两侧全部文件都属于初始化允许清单」时成立；否则必须有真实的 `commands.test`。
- 初始化允许清单包含：`.gitignore`、`README.md`、`.ghpipe/{config.json,manifest.json,AGENTS.md,SKILL.md}`、`.ghpipe/automation/**`、`.ghpipe/skills/**`、`.ghpipe/references/**`、`.ghpipe/templates/**`、`.ghpipe/plans/batch.example.json`、`docs/**/*.md`、`.github/workflows/*.yml`。
- bootstrap 时还要求：工作流文件在 head 中存在、包含 `on:`/`jobs:`/`steps:` 与至少一个 `run:`/`uses:`、且不包含符号链接或子模块。
- `--run-tests` 要求 HEAD 与当前检出一致、工作区干净，并用清洗后的环境执行 `commands.test`。
- 全零 base 只用于仓库首个提交。

#### 10.5.1 「还没有代码和测试」阶段怎么合并（避免门禁死锁）

典型场景：仓库刚接入，接下来要做的是**设计文档、开发方案、切片计划**——没有源码、没有测试脚本，跑不了业务 CI，但改动又必须进主分支。这个问题在设计上用三条规则解决：

**规则一：必需检查只有一个名字，策略随范围收紧。**

从接入第一天起，必需检查就叫 `ghpipe-quality`（由项目里的 `.github/workflows/ghpipe-quality.yml` 产生）。它在不同阶段做不同强度的校验，但**检查名不变、来源不变**，因此不会出现"新检查还没跑过就不能装成必需检查"或者"某个 PR 里必需检查压根不产生"的死锁：

| 情形 | gate 判定 | 是否需要业务测试 |
|---|---|---|
| base 是 bootstrap，且 base/head 两侧所有变更路径都在初始化允许清单内 | 初始化验证：配置结构合法 + 变更范围合法 + 工作流文件存在且结构完整 | **不需要** |
| 其它任何情形（含 base 是 strict 的文档 PR） | 业务验证 | 需要：`commands.test` 必须已登记且执行通过 |

关键约束：**PR 不能把自己从 strict 降级成 bootstrap**（判定看的是 base 的已生效策略，不是 head 自己声明）；删除、改名、类型变化都不能用来隐藏业务改动（比较的是完整文件树）。

初始化允许清单（文档阶段正好落在里面）：`.gitignore`、`README.md`、`.ghpipe/{config.json,manifest.json,AGENTS.md,SKILL.md}`、`.ghpipe/{automation,skills,references,templates,plans}/**`、`docs/**/*.md`、`.github/workflows/*.yml`。

**规则二：空仓库用 seed 先落一次地，再装规则。**

顺序很重要，否则第一个 PR 会因为没有必需检查可跑而卡住（或者规则刚装上、检查还没成功过）：

```text
1. init repo --create --empty        创建空仓库（默认分支已存在）
2. init seed --identity OWNER.json   把最小 seed（README + 最小 bootstrap 配置 + 质量工作流）作为默认分支首个提交
3. init checks --rules --check-name ghpipe-quality \
       --check-sha <seed SHA> --workflow-path .github/workflows/ghpipe-quality.yml --apply
                                    用 seed SHA 上真实成功的检查来源安装必需检查与规则
4. doctor --for bootstrap --check-sha <seed SHA>   核对真实来源与完整规则
5. 此后所有变更（文档、方案、代码）都走开发分支 + PR + 独立验收
```

seed 之所以能直推，是因为那时规则还没装；它**只**包含最小配置与质量工作流，由 Owner 署名并记录在 `execution log`，之后立刻装规则。这样既不违反"禁止在主分支开发"（这不是开发，是一次性的初始化提交），也不会留下"没有门禁的窗口"。

**规则三：文档/方案 PR 只需要"文档级"检查，但检查必须真的产生。**

- bootstrap 阶段：文档 PR 落在初始化允许清单内，`ghpipe-quality` 只做范围与结构校验即可通过，**不需要测试脚本**。独立 Reviewer 依然要验收（验收的是方案内容，不是测试通过）。
- strict 阶段：文档 PR 默认仍会跑 `commands.test`（简单、诚实）。若测试很贵，可开启 `quality.docs_fast_path: true`——仅当全部变更都在文档白名单内时跳过大测试，但必须：① 仍然执行工作流并**报告同名检查为成功**（绝不允许用路径过滤让检查消失）；② 执行一个真实的文档检查（markdown 结构 + 相对链接可达）；③ 结果里标注 `scope: docs`、`business_tests: skipped`，供 Reviewer 判断是否可接受。

三条明令禁止（都来自上一步实现的教训）：

1. **不许造必过假检查**（例如永远 return 0 的脚本）冒充业务门禁；
2. **不许在 workflow 层用路径过滤**导致必需检查在文档 PR 上不产生（GitHub 会一直等这个检查，PR 永远合并不了）；
3. **不许为了合并而把 strict 改回 bootstrap**，也不许删除或降低已有规则。

**已有项目接入**同理但更简单：登记项目**已有**的 `commands.test` 与**已有**的必需检查（来源必须取自一次真实成功的 run），接入 PR 只需满足现有门禁；本工具不新增"必须等业务测试"的前置条件，也不修改既有规则。

### 10.6 publish / release

批次路径：

- 计划文件校验：`batch`、`milestone{title,completion}`、`design_path`（安全相对路径）、`slices[]`（`slice_id` 唯一、`sequence` 严格 1..N、`depends_on` 只能是本批更早的切片、`scope/acceptance/test_requirements/deliverables` 非空、`verification[]` 含 `directory` 与 `command`）。
- `--design-ref` 必须是完整 SHA，且该 SHA 下 `design_path` 解析为文件。
- 幂等：以 `<!-- ghpipe:batch:<id> -->` 与 `<!-- ghpipe:<batch>:<slice> -->` 标记做匹配；所有既有对象的不可变内容（标题、正文、milestone）在**第一次写之前**全部比对；任何差异、重复标记、已 released/已 started 批次都不新增切片。
- 写入顺序：缺 `ghpipe:ready` 标签 → milestone → 按依赖顺序创建 Issue → 每个对象用「读对象 + 读列表」双重确认，读延时重试但绝不重试创建。
- 发布成功不释放批次；`release --milestone N --plan FILE --design-ref SHA --apply` 复核完整性与交接前置后，仅把描述里的 `ghpipe:released=false` 改为 `true`（幂等，重复调用报告已放行）。

单维护 Issue 路径：`{"key","title","body","milestone"}`，marker `<!-- ghpipe:maintenance:<key> -->`，要求里程碑存在且打开，不要求 design-ref，不使用批次 release。

缺陷（bug）路径见 §10.7：`publish --bug-file FILE` 用同一套幂等与「写前比对、写后回读」规则，但要求模板必填段完整、严重度合法，并额外要求「回归测试」信息。

### 10.7 缺陷（bug）任务类型

三类任务共存于同一生命周期，区别在**必填信息**与**验收证据**：

| 类型 | 来源 | 标记 | 特有的硬要求 |
|---|---|---|---|
| 交付切片 `slice` | 批次计划 | `<!-- ghpipe:<batch>:<slice> -->` | 方案 SHA、验收条件、验证命令、交付物 |
| 缺陷 `bug` | 缺陷模板 | `<!-- ghpipe:bug:<key> -->` | 现象/复现/影响/证据 + **回归测试**（先红后绿） |
| 维护 `maintenance` | `--issue-file` | `<!-- ghpipe:maintenance:<key> -->` | 只需 key/title/body/milestone |

#### 10.7.1 为什么必须补全缺陷类型

切片的语义是"按方案新增能力"，它的验收是"验收条件是否满足"。缺陷的语义是"某个本来应该成立的行为不成立"，它的验收是**"能在修复前复现、修复后不再复现"**——这两者不能用同一套模板，否则会出现"没有复现步骤、没有回归测试、改完就说好了"。而"AI 改完就说好了"正是本项目要解决的问题，所以缺陷必须是一等公民。

#### 10.7.2 缺陷必须提供的信息（模板字段）

按**消费者**组织，而不是按书写习惯：

| 消费者 | 需要知道 | 模板字段 |
|---|---|---|
| 修复者（Developer） | 期望 vs 实际、怎么复现、环境 | 现象、复现步骤、环境（版本/平台/commit）、稳定性（必现/偶现 + 概率） |
| 修复者 | 边界在哪 | 不包含范围（明确禁止顺手重构） |
| 验收者（Reviewer） | 怎么算修好、有没有有效回归测试 | 修复后可观察行为、回归测试要求（必须新增能复现该缺陷的测试） |
| 验收者 | 证据链 | 日志/报错/截图/CI 链接（脱敏）、最小复现命令 |
| Owner | 要不要插队、能不能放行 | 严重度、影响面（用户/功能/数据/安全）、首次出现版本或提交、是否回归 |

模板结构（`resources/templates/bug.md`，随二进制内嵌，发布时经 `publish --bug-file` 校验必填段非空）：

```markdown
<!-- ghpipe:type/bug -->
<!-- ghpipe:bug:$key -->

## 严重度
severity: blocker | critical | major | minor      # 见 §10.7.3

## 现象
- 预期：
- 实际：

## 复现
- 环境（版本 / 平台 / commit）：
- 步骤：
    1.
    2.
- 最小复现命令：
- 稳定性：必现 / 偶现（出现概率 x%）

## 影响
- 受影响用户或功能：
- 数据 / 安全影响：
- 首次出现（版本 / 提交 / 是否回归）：

## 证据
（脱敏后的日志、报错、截图链接、CI run 链接；不含 token、私钥、个人路径）

## 修复验收
- 修复后可观察行为：
- 回归测试要求：新增一个能在修复前失败、修复后通过的测试
- 不包含范围：

## 关联
（相关 Issue / PR / 上游依赖 / 讨论）
```

#### 10.7.3 严重度决定流程

| 严重度 | 含义 | 流程 |
|---|---|---|
| `blocker` | 线上不可用、数据损坏、凭据或隐私泄露 | **hotfix**：单一紧急里程碑，绕过批次放行直接领取；仍必须独立验收 + 必需检查 + 固定 SHA 合并；主会话优先插队 |
| `critical` | 核心功能不可用且无绕过 | 插入当前或下一个里程碑，需要 Owner 显式批准插队 |
| `major` | 功能受损但有绕过方式 | 正常批次排期 |
| `minor` | 体验或边缘问题 | 正常批次，或并入相关切片 |

#### 10.7.4 与切片不同的验收证据：回归测试有效性

切片可以靠"验收条件 + 真实测试"验收；缺陷必须额外证明**这个测试真的能抓住这个缺陷**，所以增加一条可机械核验的检查：

```text
ghpipe verify regression --pr <PR> [--base <SHA>] [--test <name>]
```

做法（在临时 worktree 中执行，不碰原检出、用完即删）：

1. 取 PR 的 base 与 head；把 **base 的实现** 与 **head 里改动的测试文件** 组合成一份检出；
2. 跑指定的测试命令 → **必须失败**（证明测试能复现缺陷）；
3. 换回完整的 head 检出再跑同一命令 → **必须通过**（证明修复有效）；
4. 输出结论与证据（两条命令、两次结果、涉及的测试文件），清理临时目录。

约束：只允许 Reviewer/Orchestrator 角色执行；只用只读凭据；不得修改原检出；测试路径由 `quality.test_paths` 配置（默认 `**/tests/**`、`**/test/**`、`*_test.go`、`*.test.*`、`*.spec.*`、`test_*.py`）。

**例外（否则流程会卡死）**：无法稳定复现的偶发缺陷，Issue 标注 `稳定性: 偶现` 并给出已获得的证据，此时不要求"先红后绿"，改为要求：至少一个**针对性测试或防护性断言** + Reviewer 明确写明"无法证明回归覆盖"的结论。这条例外必须在 PR 正文里显式声明，不允许静默跳过。

#### 10.7.5 缺陷的登记权限与流程

缺陷随时可能被任意角色发现，但**建 Issue 需要 Issues 写权限**（只有 Delivery 承载），因此：

| 谁发现 | 怎么做 |
|---|---|
| 主会话 / Owner | `ghpipe bug new --key <id>` 生成模板骨架 → 填写 → `ghpipe publish --bug-file FILE`（Owner 受签 publish 能力，幂等、写后回读） |
| Developer（在开发中） | 属于当前任务范围的：在 PR 正文或 Issue 评论里写明并修复；**超出当前任务范围**的：`ghpipe bug new --key <id> --draft` 只写本地草稿文件（`.ghpipe/plans/bugs/<key>.md`）并在当前 Issue 评论里留下路径，交由主会话/Owner 归档。不扩大 Developer 的应用权限 |
| Reviewer（验收中） | 验收发现的缺陷直接退回原 PR（REQUEST_CHANGES）；与本次验收无关的新缺陷同样写草稿交给主会话 |
| automation | 只读；可同步 `ghpipe:type/bug` 与严重度标签，不建 Issue |

#### 10.7.6 元数据与 done 判据

- 标签：`ghpipe:type/{slice,bug,maintenance}`（非阶段标签，PR 继承）+ `ghpipe:severity/{blocker,critical,major,minor}`（仅严重度 ≥ critical 时加，便于筛插队候选）。
- `status` 对缺陷额外输出 `type: bug`、`severity`、`stability`、`regression: {required, verified}`，其中 `verified` 来自 `verify regression` 的证据或 PR 正文里的例外声明。
- `done` 判据在通用四项（合并、main CI、清理、元数据）之外，缺陷还要求：**回归测试已验证**，或在 PR 正文里显式声明了"无法稳定复现"的例外。缺少这两者之一都停在 `closing`。
- `blocker` / `critical` 缺陷的 PR 正文还必须包含**根因分析**：为什么发生、为什么更早没被发现、同类问题还可能在哪些位置。这是 `done` 的前置之一——否则"修好了这一处、同类问题还在"会被当成完成。

### 10.8 task（单检出任务绑定）

- `claim` 前置：Issue 打开、无 `ghpipe:blocked`、有 milestone、当前分支为默认分支或已签分支、默认分支工作区干净、该 Issue 无已合并 PR、全仓无其他活动 `ghpipe/issue-*` PR。
- 绑定内容仅 `{repository, issue, branch, developer_subject, orchestrator_subject}`，不存阶段与队列；用 `hostfs.Lock` 保证本机互斥，明确声明**不是跨机器分布式锁**。
- `handoff` 必须提供精确的 `--previous-subject`，且无 pending 写。
- `release` 只能在 fresh done 或 cancelled 证据下执行（释放前再读一次事实并比对，任何变化都保留绑定）；cancelled 允许无 PR/SHA，但要求 Issue 与全部关联 PR 关闭且未合并、无 pending 写、无未提交文件；释放只删除本机绑定，不切分支、不删代码。

### 10.9 identity 与署名

- identity 记录：`{schema_version, agent_id(=subject_id), repository, tool, model, role, credential_role, created_at, session?}`，写在 `.ghpipe/state/actors/<subject_id>.json`；同主体已有不同声明时拒绝覆盖。
- 同一 repo 的重复创建是幂等读取；跨 repo 复用同一 subject 的记录会被拒绝。
- PR/Review/发布/放行的正文身份块格式统一：

````text
<!-- ghpipe:actor:start -->
### 执行身份（声明）
```json
{ ... }
```
<!-- ghpipe:actor:end -->
````

- 解析必须 fail-closed：块格式不合法即报错要求修复，绝不把畸形块当成无署名。
- 评论必须使用会话绑定的数字编号；Issue 评论只写绑定 Issue，PR 评论只写绑定 PR，并回读分支与 SHA。

### 10.10 主会话编排（Orchestrator loop）

沿用 aipipe 已验证的分工：**主会话就是 Orchestrator**，只做编排、门禁判定与收尾，不写实现、不代替验收、不借用其他角色的身份。Skill 侧对应 `ghpipe-orchestrate`，CLI 侧只提供原子操作与事实读取。

| 角色 | 做什么 | 明确禁止 |
|---|---|---|
| 主会话 / Orchestrator（delivery 凭据 + `claim`/`merge`/`cleanup`） | 读事实、领取一个任务、派发 Developer、安排独立验收、合并、收尾、释放绑定、决定下一任务 | 写代码、提交、push、Review、代替 Owner 签发、借 Developer/Reviewer 身份 |
| Developer（developer 凭据 + 独立主体） | 原地实现、有效测试、commit/push、开 PR | Review、merge、cleanup、领取下一任务 |
| Reviewer（delivery 凭据 + 另一个独立主体） | 对固定 SHA 独立验收、批准或退回 | 改实现、push、merge、cleanup |
| Owner | 建立信任、签发会话、App/仓库规则、发布/放行 | 替开发或替验收 |
| automation | 只读 + 元数据同步 | 任何交付写入 |

循环（每一步都以 GitHub 事实为准，本地不存阶段；任何一步中断后都能从 `status` 重新进入）：

1. **定位与领取**：`ghpipe task inspect` 先看本机绑定；`ghpipe status --issue N --role delivery --json` 读事实。无绑定才 `ghpipe task claim --developer-subject <uuid>`，一次只领一个；`ghpipe task next --service` 只读返回候选与排序建议，避免人工逐个翻 Issue。
2. **派发开发**：用宿主工具的**原生子 agent** 派发 Developer（§6.8）——独立上下文、独立主体身份，通过子 agent 指令把 Issue、精确分支 `ghpipe/issue-N`、Developer 会话与主体密钥路径、验收标准与禁止事项交给它。宿主不支持原生子 agent 时按 §6.8 报不兼容并建议更换工具，**不得降级为主会话亲自开发**。
3. **等待交付**：以 `status` 的 `next_actions` 为唯一权威提示；`pending/pr_missing` = 等待开发交接，不是失败。
4. **安排验收**：拿到固定 SHA 后，Owner 用 `ghpipe execution issue --work-role reviewer --issue N --auto-bind` 一条命令签发绑定该 PR/SHA 的 Reviewer 会话（自动解析 PR 与 head，不需要人工抄 SHA）。
5. **独立验收与返修**：同样用原生子 agent 派发 Reviewer（**另一个**子 agent，不复用 Developer 的上下文），它提交绑定 SHA 的 Review；退回则回原 Developer 修复。**新 head 必须重新签发并重新验收，旧批准作废**；返修轮次默认上限 3，超限报告具体阻塞。
6. **合并**：当前 SHA 具备独立批准 + 必需检查后，主会话用自身 Orchestrator 身份执行 `ghpipe pr merge <PR> --squash --match-head-commit <SHA> --json`；不使用 admin/auto、不改门禁；回读确认 `merged` 后**永不重复 merge**。
7. **收尾**：merge 自动触发 `cleanup`；失败用 `ghpipe cleanup --pr <PR>` 恢复（保护脏文件、额外提交、其他 worktree）。随后持续读 `ghpipe status --pr <PR> --role delivery --json`，直到 `closing_main_ci`、`closing_remote_absent`、`closing_local_cleanup`、`closing_metadata`、`closing_issue` 全部消失。
8. **元数据**：差异交给 Owner/automation（`ghpipe metadata --pr N --apply`），主会话只安排与回读，不自己改标签、不借 Deliver 之外的凭据。
9. **释放与下一个**：确认 done 后 `ghpipe task release`，再按已授权范围决定下一个任务。取消走同一命令：必须有 fresh cancelled 证据、无 pending 写、无未提交文件；保留分支与提交，不切 main、不删代码、不当作 done。

自动化分工（主会话不重复实现已有的自动化）：

| 由 GitHub Actions 自动完成 | 由 CLI 原子完成 | 由主会话决策 |
|---|---|---|
| 元数据标签/里程碑同步（事件 + schedule + 并发队列）、质量门禁、release 产物与 npm 发布、会话到期提醒 | 状态读取、claim/handoff/release、受签写与回读、journal 恢复、cleanup 与审计声明 | 领取哪个任务、何时派发、是否合并、何时释放；全部基于 `status` 的 `next_actions` 与门禁输出 |

主会话的可恢复性：任何一步失败都**不靠本地记忆**——重新跑 `status`/`task inspect` 即可回到正确位置；未知写先 `journal` 回读，禁止重放。

### 10.11 缺陷从哪来：三个入口

| 来源 | 谁提交 | 入口 | 落到哪里 |
|---|---|---|---|
| **用户**（产品使用者） | 外部用户，没有仓库写权限 | GitHub Issue Form：`.github/ISSUE_TEMPLATE/ghpipe-bug.yml`（由 ghpipe 生成） | 带 `ghpipe:triage` 标签的待分诊 Issue |
| **内部**（开发/验收/主会话） | 角色 agent | `ghpipe bug new` → `publish --bug-file` | `ghpipe:type/bug` 的正常任务 |
| **工具自身错误** | ghpipe 自己（opt-in） | `ghpipe report submit`（专用凭据 + 可预览） | 上报到工具仓库的 `ghpipe:report` Issue |

三个入口的共同点：**都要先过模板校验，再进同一套生命周期**；区别只在"谁有权建档"和"要不要人分诊"。

#### 10.11.1 用户提交：用 Issue Forms 做结构化入口

外部用户没有写权限，也不该被要求读我们的规范，所以入口必须是**GitHub 原生的结构化表单**（Issue Forms，`.github/ISSUE_TEMPLATE/*.yml`）：字段级必填校验、下拉选择严重度与平台、自动打标签，不需要任何服务端。

- 表单字段与 `templates/bug.md` 的必填段**一一对应**：现象（预期/实际）、复现（环境/步骤/最小命令/稳定性）、影响（范围/数据与安全/首次出现）、证据（脱敏）、修复期望（可观察行为）。
- 表单里显式提示：**不要粘贴 token、私钥、个人绝对路径、完整生产数据**；安全问题不走公开 Issue，改走 GitHub 的 Private Vulnerability Reporting（`init` 时尝试开启并回读，失败则明确提示由仓库管理员开启）。
- 生成位置是 `.github/ISSUE_TEMPLATE/ghpipe-bug.yml`：这是 `.ghpipe/` 之外的第二类固定路径例外（第一类是 `.github/workflows/ghpipe-*.yml`），生命周期与工作流相同——由 `init` 写入、`init resources` 预览升级、`removal` 指南负责移除。
- 表单默认标签只有 `ghpipe:triage`：**用户提交不等于任务**，必须分诊后才可被领取。

#### 10.11.2 分诊：把"用户报告"变成"可领取任务"

| 命令 | 谁执行 | 做什么 |
|---|---|---|
| `ghpipe triage --issue N` | 任何有 `read` 能力的角色 | 只读检查：必填段是否齐、严重度是否合法、能否复现（是否给了最小命令）、是否与既有 Issue 重复、是否属于支持范围；输出"可接受 / 缺什么 / 建议动作" |
| `ghpipe triage --issue N --accept --severity major --milestone M [--apply]` | Owner（`publish` 能力） | 追加受管结构化块 → 打 `ghpipe:type/bug` + 严重度标签 → 归入里程碑 → 去掉 `ghpipe:triage`；此后它就是普通 bug 任务 |
| `ghpipe triage --issue N --reject --reason duplicate\|out-of-scope\|insufficient-info` | Owner | 关闭并给出模板化原因；`insufficient-info` 必须列出"还缺哪几项"，便于用户补充后重新打开 |

两条硬规则：

1. **不改写用户原文**：用户提交的内容原样保留，工具只在正文尾部**追加**受管结构化块（带 `<!-- ghpipe:bug:<key> -->` 标记）；这样既能保持账本统一，又不篡改他人陈述。
2. **写后回读 + 幂等**：与其它写操作一样，重复分诊同一 Issue 不产生第二次写入；标签/里程碑以 GitHub 现状为准。

分诊本身也可以自动化一部分：`ghpipe triage --issue N --check --json` 的结论可以喂给 automation 工作流（只做校验与打标提示，不做接受/拒绝决定）；**接受任务的决定权留给 Owner**。

### 10.12 项目自动上报：把工具自身缺陷送回工具仓库

这是"用户项目自动提交 bug 到本仓库"的场景：项目里 ghpipe 出了内部错误时，把**脱敏后的固定字段**上报到工具仓库，帮我们修工具。默认关闭。

#### 10.12.1 开关与凭据

```json
{
  "reporting": {
    "enabled": false,
    "destination": "ghpipe/ghpipe",
    "credential_ref": "reporting/ghpipe",
    "max_per_hour": 3
  }
}
```

- 默认 `enabled: false`；开启需要显式配置，并用**独立凭据**（只对目标仓库有 Issues 写权限），**绝不复用业务项目的角色凭据**——这也是与角色隔离一致的要求。
- 凭据仍放仓库外（0600 / 用户配置目录 ACL），由 `doctor` 报告其可用性与"仅一个仓库"属性。

#### 10.12.2 发什么、不发什么

| 发送（固定字段白名单） | 绝不发送 |
|---|---|
| schema 版本、ghpipe 版本、动作类型（`github`/`api`/`push`/`doctor`/…）、错误分类与阶段、HTTP 状态、**脱敏端点模板**（如 `/repos/{value}/{value}/pulls`）、内部模块名与行号（最多 5 帧）、平台与架构、指纹 | 项目名/仓库名/组织名、命令参数、Issue/PR 正文、错误文本与响应正文、原始 stdout/stderr、环境变量、token/私钥、用户绝对路径、主机名 |

`ghpipe report preview` 打印**将要发送的确切内容**（等价于最终 payload 的 JSON），供人确认；不接受"看不见就发出去"。

#### 10.12.3 触发范围、去重与节流

- 触发：未捕获的内部错误、结果未知的远程写（unknown）、结构化 Review/merge 的 preflight 错误。
- 不触发：业务测试失败、只读命令（`status`/`inspect`）、`--offline`、所有非 `--apply` 的预览路径。
- 去重：指纹 = `sha256(规范化 payload)`；上报前在目标仓库按 marker `<!-- ghpipe:report:<fingerprint> -->` 搜索，命中即不创建（本地计数 +1）；列表读取触达上限时停止，不创建。
- 节流：默认每项目每小时最多 3 条；**失败的写不重试**。
- 时机：永远在原命令结束之后执行，保持原退出码与 stdout，通知只写 stderr（与 aipipe 的既有约定一致，避免上报影响主流程）。

#### 10.12.4 接收侧（工具仓库自己）

- 上报 Issue 用 `ghpipe:report` 标签区分于普通缺陷；正文是固定 schema 的 JSON 代码块 + 人类可读摘要。
- 工具仓库自带一个 triage 工作流：校验 schema 版本、按指纹去重、按分类聚类、必要时转成 `ghpipe:type/bug` 任务；不自动修改上报方任何东西。
- `docs/reporting.md` 写明"我们收什么、不收什么、多久看一次、怎么被处理"，让上报方明确预期；上报**不授予**任何写业务仓库的能力。

---

## 11. 接入体验：App 与 Skills

这一节回答三个体验问题：App 能不能全在 CLI 内搞定、Skill 装到哪里才能被 codex / zcode / opencode / pi 等工具共同发现、项目能不能复写和自定义 Skill。

### 11.1 App 的创建、安装与授权

#### 11.1.1 先说事实：哪些步骤 GitHub 没有开放 API

以下结论经 GitHub 官方文档与 REST OpenAPI 描述逐条核实（2026-09，`github/rest-api-description`）：

| 步骤 | GitHub 是否提供 API | 现实约束 |
|---|---|---|
| 创建 GitHub App | 否 | 唯一入口是 **manifest 流程**：浏览器以已登录会话 POST `github.com/settings/apps/new`，GitHub 要求人工确认创建。REST 里只有 `POST /app-manifests/{code}/conversions`，作用是把那次浏览器会话产生的临时 `code` 换成 App 凭据；它不能自己产生 `code` |
| 启用 device flow | 否 | manifest 参数表中没有该开关（参数只有 name/url/hook_attributes/redirect_url/callback_urls/setup_url/description/public/default_permissions/default_events/request_oauth_on_install/setup_on_update），只能在 App 设置页勾选。**因此 ghpipe 不依赖 device flow** |
| 创建 installation（安装到账号/组织） | 否 | 没有“创建 installation”的端点，必须由用户在安装页完成 |
| 安装范围（全部仓库 / 指定仓库） | 安装时由用户选择；安装后可用 `PUT`/`DELETE /user/installations/{installation_id}/repositories/{repository_id}` 增删单仓库 | **该端点只接受 classic PAT（`repo` scope）**，不接受 App JWT、installation token 或 fine-grained PAT；调用者需对目标仓库有 admin 权限 |
| 读取安装范围 | 是 | `GET /repos/{owner}/{repo}/installation` 与 `GET /app/installations`（需 JWT），字段 `repository_selection ∈ {all, selected}` |
| 签发角色 installation token | 是 | `POST /app/installations/{id}/access_tokens`，body 指定 `repositories` 与最小 `permissions` |
| 验证 token 范围 | 是 | `GET /installation/repositories` 必须恰好返回 1 个仓库 |

结论：**“一次浏览器都不跳”在 GitHub 侧不可实现**——创建 App 与安装这两个动作被 GitHub 明确限定为已登录浏览器会话，任何工具（包括 gh）都绕不过去。

ghpipe 能做到、也应该做到的是：把整条链路压缩成**一次浏览器会话、两次点击**，其余步骤（生成 manifest、接收回调、换取私钥、安装范围选择后的校验、签发 token、写配置、就绪检查）全部在 CLI 内自动完成，且用户**不需要手工复制 App ID、私钥、installation ID，也不需要手编 JSON**。

#### 11.1.2 路径 A（默认，推荐）：一次会话两次点击

```bash
ghpipe init app --role both            # 或 --role developer / --role delivery
```

CLI 驱动的完整流程：

1. 绑定 loopback（默认 `127.0.0.1:8954`，`--port` 可换；该端口会写进 manifest 的 `redirect_url`、`callback_urls`、`setup_url`），生成一次性 `state`，组装 manifest（`public=false`、`hook_attributes.active=false`、`default_events=[]`、`default_permissions=` 角色最小权限、`request_oauth_on_install=false`）。
2. 自动打开浏览器（本机中转页自动 POST 到 GitHub）→ 用户**点击第 1 次：Create GitHub App**。
3. GitHub 回跳 loopback 带 `code` → CLI 调 `POST /app-manifests/{code}/conversions` → 把 App `id`/`slug`/`pem`/`client_id` 写入仓库外 Owner 材料（`owner/keys/<app_id>.pem` 仅本用户可读；`owner/apps/<app_id>.json` 只存非秘密元数据与私钥路径）。
4. CLI 接着打开 `https://github.com/apps/<slug>/installations/new`（同一会话）→ 用户**点击第 2 次：Install**，并选择 **Only select repositories → 当前开发仓库**。
5. GitHub 回跳 `setup_url?installation_id=…&setup_action=install` → CLI 自动校验：App `id` 与本地一致、`POST access_tokens`（仅当前仓库 + 角色最小权限）返回的 `permissions` 等于期望、`GET /installation/repositories` 恰好 1 个且等于配置仓库、`GET /installation/repositories` 之外再用 `viewer{login}` 核对 `<slug>[bot]`。
6. 写 `config.json`（app_id / installation_id / slug / credential_ref）→ 输出 `doctor --for handoff` 摘要与后续命令示例。

`--role both` 会顺序走两遍（developer 与 delivery 各一个 App），每遍只多两次点击；也可以分两次执行，中断后重跑自动识别已完成的部分。

无浏览器 / 远程宿主：

| 场景 | 用法 |
|---|---|
| 不想自动开浏览器 | `--no-browser`：打印 URL，由用户自行打开 |
| 浏览器与 CLI 不在同一台机器（容器、沙箱、远程机） | `--paste`：CLI 提示把浏览器地址栏里的完整回跳 URL（或其中的 `code` / `installation_id`）粘回终端；`state` 单次消费，重复回调一律拒绝 |
| 可建隧道 | 文档给出 `ssh -L 8954:127.0.0.1:8954` 的做法 |
| 超时 | 默认 10 分钟，`--timeout` 可调；超时后不写任何本地状态，重跑即可 |

#### 11.1.3 路径 B（零浏览器）：复用已注册的 App

```bash
ghpipe init app --existing --role developer --app-id 123456 --private-key /protected/app.pem
```

适用于公司统一创建 App、或上一次已经建好的情况。CLI 会：读取真实 App 对象核对 owner/slug/权限集合是否恰好满足角色 → 若尚未安装则只打印安装页 URL（这一步仍需一次浏览器）→ 已安装则直接签发 token、验证范围、写配置。私钥只在项目外使用，绝不写进项目或输出内容。

#### 11.1.4 路径 C（不用 App）：角色凭据用用户 token

```bash
ghpipe auth login --credential pat --role developer --token-stdin
```

适用于企业策略禁止自建 App，或按你其他项目那样用**不同 GitHub 账号**做角色隔离的场景。必须显式接受的代价：

- 失去 App 的 bot 身份与 installation token 的单仓库收窄能力；隔离来源变成“不同账号”，`doctor` 会报告 `isolation: accounts`，不再声称依赖 App 权限；
- fine-grained PAT 与 classic PAT 都只能**在网页创建**（GitHub 同样没有创建 PAT 的 API），但只需一次粘贴，之后全部在 CLI 内；
- `doctor` 改为核对 token 对当前仓库的可见性与写权限（`GET /repos/{repo}` + `permissions` 字段），并明确标注「无法证明 token 仅能访问一个仓库」，这一条不达标就如实报告，不伪装成等价方案。

#### 11.1.5 安装范围：全部仓库还是指定仓库

| 选项 | 建议 | 理由 |
|---|---|---|
| Only select repositories → 当前仓库（`repository_selection: selected`） | **推荐** | 最小权限；ghpipe 签发时本来就只申请单仓库 token；`doctor` 能验证「可见仓库恰好 1 个」 |
| All repositories（`all`） | 可用但无收益 | 我们的 token 仍收窄到单仓库，但 App 权限漂移时暴露面更大；`doctor` 输出 `warn: installation covers all repositories` |
| 多仓库 selected | 按需 | 仅当同一个 App 服务多个仓库；每个仓库仍需各自 `config.json` 与各自的验证 |

```bash
ghpipe init app repos                        # 只读：当前 repository_selection、可见仓库、修正指引
ghpipe init app repos --remove OTHER/REPO --token-stdin
ghpipe init app repos --add OWNER/REPO --token-stdin
```

`--token-stdin` 要求一次性 classic PAT（`repo` scope，且调用者对仓库有 admin），**用后即弃、不落盘、不进 config**；这是 GitHub 官方唯一允许的自动化途径，CLI 会直接说明这一约束，不会静默改用其他凭据或降低验证标准。

#### 11.1.6 为什么不做“官方共享 App”

若由 ghpipe 项目方提供一个公共 App（用户只安装、不自建），体验最省事，但意味着私钥由第三方持有、安装范围是组织级/多仓库、用户无法本地轮转信任，与「每个项目独立、最小权限、可移除」的核心目标冲突。仅作为明确要求时的扩展（`--shared-app`），不进首版。

#### 11.1.7 App 与凭据相关命令

```text
ghpipe init app --role developer|delivery|both [--no-browser|--paste] [--port N] [--timeout DUR] [--apply]
ghpipe init app --existing --role R --app-id N --private-key FILE
ghpipe init app repos [--add REPO | --remove REPO --token-stdin]
ghpipe auth login --credential pat --role R --token-stdin
ghpipe auth trust-app --role R --app-owner OWNER --permissions-file FILE   # 复用共享 App 的显式信任（内容不变）
ghpipe auth token --role R [--status|--refresh]
```

### 11.2 Skill 分发：装在哪、怎么被各工具发现

#### 11.2.1 官方依据（OpenAI / Codex 文档，2026-09 抓取）

| 来源 | 与本设计直接相关的结论 |
|---|---|
| [Build skills](https://learn.chatgpt.com/docs/build-skills) | 技能 = 目录 + `SKILL.md`（**必须**含 `name`、`description`）+ 可选 `scripts/`、`references/`、`assets/`、`agents/openai.yaml`；遵循 open agent skills standard；渐进披露：宿主先拿 name+description，选中后才读 `SKILL.md` 全文 |
| 同上（加载位置表） | Codex 从四类位置加载：repo 级 `$CWD/.agents/skills`、`$CWD/../.agents/skills`、`$REPO_ROOT/.agents/skills`；用户级 `$HOME/.agents/skills`；管理员级 `/etc/codex/skills`；系统内置。**官方明确支持符号链接的技能目录，并跟随链接目标** |
| 同上（上下文预算） | 初始技能清单最多占上下文 **2%**（上下文未知时 8,000 字符）；技能多时先压缩 description，再超出会**省略部分技能并告警** |
| 同上（同名） | 同名技能**不合并**，可能同时出现在选择器里 |
| [Customization](https://learn.chatgpt.com/docs/customization/overview) | 分工：`AGENTS.md` 塑造行为、skills 打包可复用流程；"Put repo skills in `.agents/skills` when the workflow applies to that project; use your user directory for skills you want across all repos" |
| [AGENTS.md](https://learn.chatgpt.com/docs/agent-configuration/agents-md) | 从项目根向下走到 CWD 逐目录合并，**越靠近 CWD 越优先**；合并总预算 `project_doc_max_bytes` 默认 **32 KiB**；官方要求 "Keep it small" |
| [Skills & Plugins](https://learn.chatgpt.com/docs/skills-and-plugins) | skill 是创作格式，plugin 是分发单位；跨仓库/团队分发才需要 plugin |

据此对原设计做三处修正：

1. **项目级落点直接用 `.agents/skills`**：这是官方定义的项目级发现目录，不需要发明路径；ghpipe 往里面写**真实文件**（见下条）。
2. **用户级只放一个路由技能 `ghpipe`，不再放 7 个 `ghpipe-*` 转发**：官方说明同名技能不合并、可能同时出现，所以用户级若与项目级同名会造成重复与歧义；而单个路由技能只占一条上下文预算。
3. **把上下文预算当设计约束**：description 前置触发词、控制在 ~80 字符内；用户级最多 1 条；并提供「禁用而不删除」的途径。

#### 11.2.2 设计原则与三层机制

设计原则：

1. **内容只存一份**：Skill 正本永远在项目内 `.ghpipe/skills/`，随代码评审、随版本升级；不复制到 home，也不在 home 维护第二份流程。
2. **落盘只是优化，不是前提**：技能内容同时嵌在二进制里，任何一个工具都能用 `ghpipe skills read <name>` 拿到与当前版本一致的内容——**这是 Windows 上的根解，也是落盘失败时的兜底**（对齐 larksuite/cli 的做法，见 §11.2.7）。
3. **工具侧链接优先、拷贝兜底**（与 `skills add` 相同的策略）：能建相对符号链接就建（正本唯一、更新即生效），建不了就覆盖式拷贝；两种模式都记录在状态文件里，`skills status` 明确区分。
4. **入口位置在仓库根**（git root），因为 `$REPO_ROOT/.agents/skills` 对仓库内任意子目录都可见；monorepo 需要更贴近子服务时用 `--at DIR`。

| 机制 | 形态 | Windows 可用性 |
|---|---|---|
| `embedded-read`（**主路径**） | `ghpipe skills list` / `ghpipe skills read <name>[/path]` 直接读二进制内嵌内容，并在启动提示里给出入口 | 永远可用，不依赖文件系统 |
| `agents-md` | 仓库根 `AGENTS.md` 的受管块（2–3 行）：本仓库 ghpipe 入口见 `.ghpipe/AGENTS.md`，技能用 `ghpipe skills read` 或读 `.agents/skills/` | 永远可用（纯文本写入） |
| `project-skill-dir` | `<repo>/.agents/skills/<name>` → `../../.ghpipe/skills/<name>` 相对符号链接（Claude Code 用 `.claude/skills`，ZCode 用 `.zcode/skills`）；链接不可用时同路径改为覆盖式拷贝 | 链接需开发者模式；否则自动拷贝 |
| `user-router` | 唯一一个用户级技能 `~/.agents/skills/ghpipe/`：「当前仓库有 `.ghpipe/` 时，读它的 `.ghpipe/AGENTS.md`，技能内容用 `ghpipe skills read`」 | 同上 |

5. **路径写成可配置表**（`~/.config/ghpipe/tools.json`，内置默认值），因为各工具约定变动频繁，用户可增改而不必等 ghpipe 发版。

#### 11.2.3 各工具落点

| 工具 | 用户级目录 | 项目级目录 | 依据 |
|---|---|---|---|
| Codex | `~/.agents/skills/<name>/` | `.agents/skills/`（CWD 起向上到 repo root 逐层扫描） | 官方文档位置表 |
| Codex（管理员） | `/etc/codex/skills/`（镜像/CI 场景可选） | — | 官方文档位置表 |
| pi | `~/.pi/agent/skills/<name>` | `.agents/skills/` | 本机实测（目录内为指向 `~/.agents/skills` 的相对符号链接） |
| ZCode | `~/.zcode/skills/<name>` | 未验证 | 本机实测：同样是符号链接 |
| OpenCode | `~/.config/opencode/skills/<name>` | `.opencode/skills/` | 上游文档约定，本机未安装、未验证 |
| Claude Code | `~/.claude/skills/<name>` | `.claude/skills/` | 上游文档约定，本机未安装、未验证 |
| 其他/未知工具 | — | — | 走 `agents-md` 兜底 |

你本机已经形成「`~/.agents/skills` 正本 + 各工具符号链接」的既有约定（Codex 官方也明确支持跟随链接），ghpipe 沿用同一形态：项目级在仓库根建 `.agents/skills/<name>` 指向 `.ghpipe/skills/<name>` 的链接（不可用时拷贝），用户级只写一个 `ghpipe` 路由技能。你原有的、不属于 ghpipe 的链接会被标成 `foreign`，只读观察不做覆盖。

表中 `~` 在 Windows 上的对应位置是 `%USERPROFILE%`（例如 `%USERPROFILE%\.agents\skills`）；`/etc/codex/skills` 在 Windows 上没有对应项，管理员级落点由工具自身约定决定（未验证）。

为什么不把 Skill 装到 `~/.ghpipe/skills` 或 `~/.config/ghpipe/skills`：没有任何工具会去读那些路径，放进去等于要求用户逐工具改配置，反而增加跳转与漏配。`~/.config/ghpipe/` 只承担 ghpipe 自己的东西（凭据注册表、Owner 材料、`tools.json` 路径表）。

#### 11.2.9 与官方建议逐条对照

依据：OpenAI 官方 Skills / Customization / AGENTS.md 文档（§11.2.1）以及它们引用的 [open agent skills 规范](https://agentskills.io/specification)。左列是官方要求或建议，右列是 ghpipe 的落实方式；**加粗**项是本次补齐的缺口。

| 官方要求/建议 | ghpipe 落实 |
|---|---|
| `SKILL.md` 必含 `name` + `description` | `skills lint` 校验；`skills new` / `clone` 生成合规骨架 |
| `name`：1–64 字符、仅小写字母数字与连字符、不能首尾连字符、不能连续连字符、**必须等于父目录名** | **`skills lint` 逐条校验（含"目录名一致"）**；`skills clone` 自动改写为合法名 |
| `description`：1–1024 字符、说明做什么与何时用、包含关键词 | 校验长度与非空；**另加更严的写作规则**：单行、约 80 字符、前置触发词（受 Codex 清单 2%/8000 字符预算约束） |
| 可选字段 `license` / `compatibility`（≤500）/ `metadata`（string→string）/ `allowed-tools`（实验性） | 支持透传：`license`、`compatibility` 由项目填写；`metadata.version` 由工具维护；**不依赖 `allowed-tools`**（实验性，各实现支持不一） |
| 渐进披露：启动只读 metadata（约 100 tokens）；激活后读整个 `SKILL.md`（建议 <5000 tokens、**正文 <500 行**）；资源按需加载 | 写作规则 + `skills lint` 检查行数上限；机制文档下沉到 `references/` |
| 文件引用用相对路径、**只允许一层深**、避免深链 | `skills lint` 检查正文中的相对引用深度；`skills read` 支持 `name/references/x.md` 形式 |
| `scripts/` 要自包含、说明依赖、错误信息可读、处理边界 | 写作规则；**并明确"脚本不内嵌"的冲突处理**：带 `scripts/` 的技能必须走拷贝模式，`skills read` 对脚本返回"未落盘"提示并建议 `skills sync` |
| `references/` 保持聚焦、单文件小 | 写作规则，按主题拆分（现状已如此） |
| 官方验证器 `skills-ref validate` | **内置等价校验 `ghpipe skills lint`**，并在文档给出用 `skills-ref` 复核的命令 |
| `agents/openai.yaml` 的 `interface`（display_name、short_description、icon_small/large、brand_color、default_prompt）、`policy.allow_implicit_invocation`、`dependencies.tools` | **补齐字段支持**：默认填 `display_name` + `short_description` + `policy`；图标与 `default_prompt` 可选；`dependencies.tools` 仅当技能确实依赖 MCP 时声明（首版无） |
| 官方 best practice：一个技能只做一件事；能用指令别用脚本；祈使句 + 明确输入输出；**用真实提示词测试 description 触发** | 前三条已进写作规则；**新增触发回归**：`resources/skills-triggers.json` 保存"提示词 → 期望技能"用例，`skills lint --triggers` 做离线一致性检查（关键词级，不调用模型） |
| Codex 位置表：repo `.agents/skills`（CWD→root）、user `~/.agents/skills`、admin `/etc/codex/skills`；**支持符号链接并跟随目标** | 完全对齐（§11.2.2/11.2.3）：项目级落 `.agents/skills`，用户级只放 1 条路由技能；链接优先、拷贝兜底 |
| 同名技能不合并、可能同时出现 | 建入口时按「本地层 > 项目层 > 内嵌层」解析，保证同一路径下只有一个同名技能 |
| 用 `[[skills.config]]` 禁用而不删除 | `skills disable --apply` 生成该片段；默认只打印 |
| 分发：repo 内工作流用 skill 目录，跨仓库/团队分发用 plugin | 首版只用 skill 目录（技能依赖本仓库与本地二进制，云端 plugin 无意义）；plugin 记为将来选项 |

#### 11.2.4 技能的元数据、预算与禁用

- 每个技能随正本附带 `agents/openai.yaml`：

```yaml
interface:
  display_name: "ghpipe develop"
  short_description: "开发已绑定的 Issue 并交付 PR"
policy:
  allow_implicit_invocation: true    # Owner 类操作（init/publish/release）设为 false
```

- `description` 写作规则（受 2%/8,000 字符预算约束）：前置触发词、单行、约 80 字符内，例如 `开发或继续已绑定的 Issue，交付实现与 PR；不领取任务、不验收。` 不写背景故事，不列全部功能。
- 用户级只保留 `ghpipe` 一条路由技能，避免技能清单膨胀；仓库级 7 条具名技能是「用户在当前仓库工作时」才可见，正好落在各自场景。
- **禁用而不删除**：优先用 `ghpipe skills sync --exclude <name>`（不为该技能建入口，正本保留）；Codex 侧还可用官方开关：

```toml
[[skills.config]]
path = "/abs/repo/.ghpipe/skills/ghpipe-review/SKILL.md"
enabled = false
```

`ghpipe skills disable <name> --tool codex` 默认只打印这段片段，`--apply` 才写入 `~/.codex/config.toml`（不静默改用户配置）。

#### 11.2.5 为什么不打包成 plugin

官方指引是「repo 内工作流用 skill 目录；要跨仓库/团队分发或与连接器一起发布，再打包成 plugin」。ghpipe 的技能天然依赖本仓库文件与本地 `ghpipe` 二进制，在 Chat/Work 的云端环境里没有意义，因此首版只做 repo-scoped skill 目录（这也是官方给 repo 工作流推荐的形态）。将来若要跨团队分发，可再打包一个 plugin（内含路由技能，必要时配 MCP 连接器），不影响现有设计。

#### 11.2.6 命令

```text
ghpipe skills list                          # 列出技能（名称、描述、来源层、是否内嵌）
ghpipe skills read <name>[/<path>]          # 读内嵌/项目/本地层的有效内容，与二进制同版本
ghpipe skills sync --project [--at DIR]     # 在仓库根（或指定目录）建/刷新发现入口（链接优先，失败转拷贝）
ghpipe skills sync --user                   # 建/刷新唯一的路由技能 ~/.agents/skills/ghpipe
ghpipe skills sync --all [--tool T]         # 两者都做；--tool 限定单个工具
ghpipe skills sync --link | --copy          # 强制指定放置方式（默认 auto：能链接就链接）
ghpipe skills sync --layout separate|suite   # separate=逐技能；suite=聚合为单一入口技能（省上下文）
ghpipe skills uninstall --user              # 移除路由技能与各工具入口，不动项目正本
ghpipe skills disable <name> [--apply]      # 生成/写入官方禁用片段
ghpipe skills status                        # 各工具入口状态（linked/copied/modified/missing/foreign/stale）与版本漂移
ghpipe skills lint [<name>] [--triggers]    # 按 open agent skills 规范校验 frontmatter/命名/行数/引用深度/触发用例
```

`ghpipe init repo` 结束时默认只做 `agents-md`（零风险、全工具通用），并提示一句 `ghpipe skills sync --all` 可以补原生发现；不会未经许可往 home 目录写东西。

#### 11.2.7 参考实现：larksuite/cli 的做法（含 Windows）

同为「CLI + 一批 Agent Skills」的产品，larksuite/cli 的处理方式值得直接借鉴，它对 Windows 的回答是**不依赖落盘**：

| 它的做法 | 证据 | ghpipe 的对应设计 |
|---|---|---|
| 技能内容 `go:embed` 进二进制，提供 `lark-cli skills list` / `skills read <name>[/path]`，保证与 CLI 版本同步；`assets/`、`scripts/` 不嵌入 | `content_embed.go`、`cmd/skill/skill.go` | `ghpipe skills list/read` 作为主路径；内嵌 `references/`，机器资源（`scripts/`、`templates/`）仍只落盘 |
| 安装到各工具目录**外包给生态安装器**：`npx -y skills add <source> -g -y`（npm 包 `skills`，vercel-labs/skills） | `internal/selfupdate/updater.go`、`scripts/install-wizard.js` | `ghpipe skills sync` 内置实现为主；`--installer npx-skills` 可选委托，不做硬依赖 |
| 只做**漂移检测与提示**：启动时读本地 `skills-state.json`（零网络、零子进程），版本不一致就追加一行 `run: lark-cli update`；CI / DEV 版本 / `LARKSUITE_CLI_NO_SKILLS_NOTIFIER` 自动跳过 | `internal/skillscheck/{check,state,notice,skip}.go` | `state/skills-state.json` + 启动单行提示（`ghpipe skills sync` 修复），CI 与 dev 构建跳过，环境变量可关闭 |
| **聚合布局** `suite`：把 26 个技能合并成一个 `lark-suite` 入口，按需再读细节 | `internal/skillscheck/layout.go`、`isolated-skills/lark-suite` | `skills sync --layout suite`：只装一个 `ghpipe` 入口技能，细节由 `ghpipe skills read` 提供；既省上下文预算又少落盘 |
| npm 包装层的 Windows 细节：`os: [darwin, linux, win32]`、`cpu: [x64, arm64, riscv64]`；Windows 用 `.zip` + PowerShell `Expand-Archive`（`tar` 兜底）；`checksums.txt` 校验；`--ssl-revoke-best-effort` 处理 Schannel 证书撤销离线；`run.js` 处理自更新留下的 `.old` 二进制 | `package.json`、`scripts/install.js`、`scripts/run.js` | 采纳到 §13.2 |
| **更新即覆盖**：版本变化时对 `ToUpdate` 集合重新执行 `skills add`（覆盖安装），再 `skills remove` 掉不再官方的技能，并回读 `skills ls -g --json` 用 `assertSameSkillNames` 校验集合一致 | `internal/skillscheck/sync.go`（`syncLayout`、`removeSkills`、`fallbackSeparate`） | `ghpipe skills sync` 同样是「重新安装即覆盖 + 删除退出集合的入口 + 回读校验入口集合」，语义对齐 |

放置方式上，`npx skills add` **默认建符号链接**（"Creates symlinks from each agent to a canonical copy. Single source of truth, easy updates."），拷贝是它提供的 `--copy` 备选（"Use when symlinks aren't supported"）；你机器上 `~/.pi/agent/skills`、`~/.zcode/skills` 指向 `~/.agents/skills` 的链接就是这么来的。所以飞书的完整策略是：**内嵌内容兜底 + 链接放置（拷贝兜底）+ 重新安装即覆盖 + 清理退出项 + 回读校验集合 + 漂移提示 + 可切换布局**。

ghpipe 逐条沿用这套策略，只做一处必要适配：**正本位置**。飞书的正本是安装器写下的 `~/.agents/skills/<name>`，链接由各 agent 目录指向它；我们的正本在项目内 `.ghpipe/skills/`（要随代码评审、要支持项目复写），因此链接由工具目录指向项目内的正本。其余语义一致：默认链接、失败拷贝、覆盖式更新、退出项清理、集合回读校验、状态文件与启动提示。

它的 agent 路径表（codex / claude-code / cursor / gemini-cli / copilot / amp / zed / droid / kilo …）也说明「路径表外置、可更新」是必需设计，而不是可选优化。

因此 ghpipe 的结论是四句话：**读技能不依赖落盘；放置链接优先、拷贝兜底；更新一律重新安装覆盖 ghpipe 写过的入口并清理退出项；任何情况下都还有 `ghpipe skills read` 与 `AGENTS.md` 兜底。**

#### 11.2.8 更新语义（版本更新与技能更新走同一条路径）

| 触发 | 行为 |
|---|---|
| CLI 升级（`ghpipe update`、npm 包装层自动补装、或手动替换二进制） | 新内嵌版本在下次同步时生效；`ghpipe update` 会在更新二进制后自动执行第 4 步的入口同步，启动漂移提示也会提示 |
| 项目技能升级（`ghpipe init resources --apply`） | 更新 `.ghpipe/skills/` 中**未被复写**的受管文件；命令结束默认接着执行一次 `skills sync`（可用 `--no-sync` 跳过），把工具目录入口一并刷新到新版本 |
| 技能内容变更（项目复写 / 自定义 / 本地层） | `skills read` 立即反映；链接模式下工具侧立刻生效，拷贝模式下由 `skills sync` 覆盖 |
| 技能被移除或改名 | 按状态文件确认入口由 ghpipe 写入（`linked` / `copied` 且哈希一致）后删除；`modified` / `foreign` 保留并报告，不静默清理 |
| 同步中途失败（文件被占用、权限不足） | 退避重试后明确报错；拷贝模式逐文件原子替换，不留半成品；链接模式失败即报错，不留下悬空链接 |

一句话：**安装、升级、复写之后都只需要一次 `ghpipe skills sync`——它是幂等的「重新安装即覆盖」，并且结束后回读入口集合做校验；`ghpipe update` 只是把「更新二进制 + 这一步同步」串成一条命令。**

### 11.3 项目级复写与自定义

分层与优先级（高 → 低）：

| 层 | 位置 | 入 Git | 用途 |
|---|---|---|---|
| 本地层 | `.ghpipe/skills.local/<name>/` | 否（工具自动写入 `.gitignore`） | 个人试验、机器专属调整；同名时遮蔽下层 |
| 项目层 | `.ghpipe/skills/<name>/` | 是 | 官方 Skill 的**就地复写**，或全新自定义 Skill；同名时覆盖内置 |
| 内置层 | 二进制内 `resources/skills/` | —（随版本） | 官方基线，不落盘 |

**同名只允许出现一次**：官方说明「同名技能不合并，可能同时出现在选择器里」，所以层叠在**写入入口时**解析：`skills sync` 为每个技能名只建一个入口，内容取自命中的最高层（本地层 > 项目层 > 内嵌层）。绝不在同一发现路径下放两个同名技能，也不把同一名字同时装到用户级与项目级。

`ghpipe skills read` 走同一个解析顺序（本地层 > 项目层 > 内嵌层），并在结果里报告 `source_layer`，因此**项目复写后的内容对 Agent 立即可见**，不需要等 `skills sync` 重新落盘；内嵌层作为最终兜底，保证任何平台上都读得到。

**“复写”与“自定义”不需要额外清单**：`.ghpipe/manifest.json` 已记录每个受管文件的安装时 sha256。

| 状态 | 判定 | 升级时行为 |
|---|---|---|
| managed | 哈希 == manifest 记录 | 正常升级到新版 |
| modified（复写） | 路径在 manifest 内但哈希不同 | **绝不覆盖**；写出 `SKILL.md.new` 并列出三方差异（安装基线 / 当前 / 新版），提示用 `ghpipe skills diff` 合并 |
| custom（自定义） | 路径不在 manifest 内 | 永久保留，升级完全不触碰 |
| retired | manifest 内有、新版不再产出 | 仅当内容仍等于 manifest 哈希时删除，否则保留并报告 |

命令面：

```text
ghpipe skills list                 # name / 来源层 / 状态(managed|modified|custom) / frontmatter 描述
ghpipe skills new <name>           # 生成骨架：frontmatter(name,description) + 前提/步骤/停止条件模板
ghpipe skills clone <src> <dst>    # 复制并改写 frontmatter 的 name，用于“基于官方 Skill 派生自己的版本”
ghpipe skills diff <name>          # 三方差异：内置基线 ↔ 当前 ↔ 待升级版本
ghpipe skills reset <name>         # 丢弃本地复写、恢复基线（需显式 --yes）
```

工程约束：

- `skills` 只允许操作 `.ghpipe/skills/`、`.ghpipe/skills.local/`、工具入口目录与根 `AGENTS.md` 的受管块；路径解析后越界即拒绝（`EvalSymlinks` 校验，防链接逃逸）。
- 工具侧默认建相对符号链接，链接不可用时改为覆盖式拷贝；两种模式都记录在 `.ghpipe/state/skills-state.json`（`tool`、`path`、`mode: link|copy`、`target`、`source_layer`、`content_hash`、`cli_version`、`synced_at`）。覆盖规则：实际形态与记录一致（`linked`/`copied`）就直接覆盖；`modified`（拷贝后内容被改过）默认保留并报告，`--force` 才覆盖；`foreign`（不是 ghpipe 写的，含用户原有链接）默认拒绝，`--force` 才覆盖；技能退出受管集合时只在记录匹配时删除，否则保留并报告。
- `doctor --for plan|develop|review` 输出 `skills: {managed, modified, custom, drift}`，Agent 与用户一眼知道当前生效的是哪一层。
- Skill 是给 Agent 的流程说明，**不是权限来源**：Skill 文件、AGENTS.md 受管块、路由技能与工具侧入口都不影响会话能力判定；文档与 Skill 头部都要写明这一点，避免「改 Skill 即提权」的误解。
- 工具目录里的副本**不要手改**：改了会被识别为 `modified` 并在下次同步时提示（`--force` 才会覆盖）；要定制就改 `.ghpipe/skills/` 或放 `.ghpipe/skills.local/`，改完 `ghpipe skills sync` 即可，且不需要重装 CLI。
- Skill 可以带自己的 `references/` 与 `scripts/`（本机已有先例），随正本一起被工具发现，升级与复写规则同上。
- 编写遵循官方 best practices：一个技能只做一件事；能用指令表达就不用脚本（只有需要确定性或外部工具时才放 `scripts/`）；步骤用祈使句并写清输入与输出；description 要拿真实提示词回归验证触发是否准确。
- 技能目录结构对齐官方约定：`SKILL.md` + 可选 `scripts/`、`references/`、`assets/`、`agents/openai.yaml`；`skills new` 与 `skills clone` 生成的骨架必须符合这一结构，且 frontmatter 只有 `name`、`description` 为必填。

### 11.4 内嵌资源清单

```
resources/
├── AGENTS.md                      # 目标项目入口导航（写明场景 → Skill 路由）
├── SKILL.md                       # 目标项目 Skill 元数据
├── README.md                      # 目标项目内 ghpipe 说明
├── skills/
│   ├── ghpipe-init/SKILL.md       # 接入：repo/app/checks/metadata/resources/seed
│   ├── ghpipe-plan/SKILL.md       # 方案与切片（不触碰远端）
│   ├── ghpipe-publish/SKILL.md    # 发布批次或单维护 Issue、放行
│   ├── ghpipe-develop/SKILL.md    # 指定 Issue 开发到 PR
│   ├── ghpipe-review/SKILL.md     # 固定 SHA 独立验收
│   ├── ghpipe-orchestrate/SKILL.md# 主会话编排与收尾
│   └── ghpipe-resume/SKILL.md     # 中断/换主体/未知写恢复
│       （每个技能目录另含 agents/openai.yaml：显示名、短描述、隐式调用策略）
├── router/SKILL.md                # 用户级唯一路由技能，安装为 ~/.agents/skills/ghpipe
├── references/
│   ├── cli.md, configuration.md, session.md, identity.md, lifecycle.md,
│   ├── metadata.md, status.md, quality.md, github.md, apps.md,
│   └── testing.md, contributing.md, documentation.md, removal.md, tools.md
├── templates/
│   ├── ghpipe-quality.yml, ghpipe-metadata.yml, ghpipe-review-signal.yml,
│   ├── issue.md（切片）, bug.md（缺陷）, pr.md
│   └── ghpipe-bug.yml（用户提交用的 Issue Form，落到 .github/ISSUE_TEMPLATE/）
└── plans/batch.example.json
```

Skills 内容按新命令与新路径重写：所有 `aipipe` → `ghpipe`、`.aipipe/` → `.ghpipe/`、`aipipe/issue-N` → `ghpipe/issue-N`、`--execution-session` → `--session`、`receipt` → `journal`、`github -- …` → `pr …`。Skill 的「前提 / 步骤 / 停止条件 / 按需阅读」结构保留——这是当前实现中被验证有效的部分（Skill 自含日常步骤，机制下沉到 references）。

嵌入策略（决定 `ghpipe skills read` 能读到什么）：

| 内容 | 是否嵌入二进制 | 说明 |
|---|---|---|
| `AGENTS.md`、`SKILL.md`、`router/SKILL.md`、`skills/*/SKILL.md`、`skills/*/references/**`、`skills/*/agents/openai.yaml` | 是 | Agent 可读的文档类内容，必须与二进制同版本；`skills read` 与启动提示都依赖它 |
| `templates/**`、`plans/**`、`skills/*/scripts/**`、`skills/*/assets/**` | 否（仅落盘） | 机器资源：由 `init`/`skills sync` 写入项目，版本一致性由 `manifest.json` 哈希负责 |

这与 larksuite/cli 的取舍一致（它的注释写明 "Machine-resource skill dirs (assets/, scripts/) are excluded"）：文档必须随二进制走，机器资源按项目落盘。

### 11.5 scaffold、升级与退休

```bash
ghpipe init resources                # 预览：逐文件 before/after 哈希 + 整体 digest
ghpipe init resources --reviewed-digest DIGEST --apply
```

- 预览列出：`changes`（新增/修改）与 `preserved`（本地已修改、非受管路径、符号链接）。
- `--apply` 必须携带预览输出的 digest；应用前逐个复核目标文件哈希未变，任一变化即整体拒绝，不做部分应用。
- 退休只删除 `manifest.json` 记录过、内容哈希一致、且位于受管前缀内的文件；其余一律保留并报告原因（`unmanaged_path`、`modified`、`symlink_or_non_regular`）。
- 生成后重写 `manifest.json`（新版本 + 新哈希表）。
- 资源升级只触碰 `.ghpipe/` 下的受管资源（`skills/`、`references/`、`templates/`、`plans/batch.example.json`、`AGENTS.md`、`SKILL.md`、`README.md`、`manifest.json`）；不触碰 `config.json`、`skills.local/`、`state/`、业务目录，也不触碰 `.github/workflows/`（工作流与该文件必须分开核对，由 Owner 经正常 PR 安装）。

### 11.6 用户级入口与工具探测

- 用户级入口由 `ghpipe skills sync --user` 建立（§11.2），只有一个名为 `ghpipe` 的路由技能，内容只有「确认当前业务仓库 → 读它的 `.ghpipe/AGENTS.md` → 按场景读对应技能」，不含流程细节，也不会与项目级技能同名冲突。
- 工具探测顺序：读 `~/.config/ghpipe/tools.json`；缺失时用内置默认表；对每个工具先判断其用户级目录是否存在（存在即视为已安装），再用 `--tool` 显式指定可强制创建。
- `ghpipe skills status` 报告「哪些工具已接入、哪些链接悬空、项目正本是否更新后未同步」，并作为 `doctor` 的附加信息输出。
- ghpipe 不修改任何工具的配置内容（不写 codex/opencode/claude 的配置文件），只增删自己创建的链接与那一个路由技能；唯一例外是 `skills disable --apply`，它需要显式确认才写 `~/.codex/config.toml`。

---

## 12. 自动化与 CI

### 12.1 目标项目工作流

| 工作流 | 触发 | 权限 | 行为 |
|---|---|---|---|
| `.github/workflows/ghpipe-quality.yml`（业务必需检查） | `push`、`pull_request` | `contents: read` | `ghpipe quality check --base $BASE --run-tests` |
| `.github/workflows/ghpipe-metadata.yml` | `pull_request_target`、`issues`、`create`/`delete`、`schedule`（17/47）、`workflow_dispatch`、`workflow_run` | `contents: read`、`issues: write`、`pull-requests: write`、`actions: read` | `ghpipe metadata workflow`（默认只读；装了受签 automation 会话才写） |
| `.github/workflows/ghpipe-review-signal.yml` | `pull_request_review` | 无 | 只产生一个轻量 run，供 metadata 工作流通过 `workflow_run` 消费 |

关键安全点保留：metadata 工作流固定 checkout 默认分支的 `.ghpipe/automation`，不使用 PR 源码、不使用缓存、不下载 artifact、不运行 PR 代码；`pull_request_review` 事件本身不执行可信写入。

### 12.2 二进制获取方式（工作流内）

工作流必须拿到**固定版本、可校验**的二进制。方案（推荐 A，备选 B）：

| 方案 | 步骤 | 优点 | 代价 |
|---|---|---|---|
| **A. npm 包装层**（推荐） | `npx --yes ghpipe@0.1.0 …` | 与用户安装同一分发通道、同一校验逻辑；npm 版本不可变 | 需要 runner 上的 npm（ubuntu-latest 自带） |
| B. Release 直连 | `curl -sSL …/ghpipe_0.1.0_linux_amd64.tar.gz` + 校验 `checksums.txt` | 不依赖 npm registry | 需在 workflow 里维护下载与校验脚本（可复用同一段 shell） |

两种方式都要求：版本号写死在工作流里（不使用 `latest`），校验 sha256，失败即 job 失败。Owner 审查 workflow 时看到的就是将要执行的精确版本。

### 12.3 automation 会话

- Owner 为 Actions 的固定检出目录签发 `work_role=automation`、能力仅 `read` + `metadata` 的短期会话，通过仓库 secrets 安装：
  `GHPIPE_METADATA_SESSION`、`GHPIPE_AUTOMATION_SUBJECT_KEY`、`GHPIPE_EXECUTION_AUTHORITY_PUBLIC_KEY`。
- 工作流启动时把信任根写入 `$HOME/.config/ghpipe/execution-authority.pub`，把会话与主体密钥写入 `$RUNNER_TEMP`（仅本用户可读），并 unset secrets 相关变量，然后执行 `ghpipe metadata workflow`。
- `GITHUB_TOKEN` 只是传输凭据，不构成授权；未安装/过期/签名无效时工作流明确报告「待 Owner 轮转」，绝不回落到 Delivery/Owner 凭据。

### 12.4 本仓库（ghpipe 自身）的 CI

| 任务 | 内容 |
|---|---|
| `test` | 三平台矩阵：`ubuntu-latest` / `macos-latest` / `windows-latest` 各跑 `go vet ./...` 与 `go test -count=1 ./...`（已落地为 `.github/workflows/ci.yml`）；`-race` 只在 Linux 与 macOS 跑（Windows 上 CGO 限制） |
| `build` | 交叉编译矩阵：`CGO_ENABLED=0` 覆盖 darwin/linux/windows × amd64/arm64（同一 workflow 内的 `build` job；32 位目标将来单独 job，允许失败不阻断） |
| `npm-wrapper` | 用本地假 Release 起 `httptest`，跑 `npm pack` + 安装 + `ghpipe --version` 校验 |
| `resource-digest` | 校验 `resources/` 与 `manifest` 生成逻辑一致，防止内嵌资源与文档漂移 |
| `platform` | 平台专属用例：Windows 上的锁/原子替换/拷贝回退/路径策略（UNC、设备命名空间、保留设备名）、macOS/Linux 上的 0600 校验与符号链接、CRLF 工作区下的资源哈希一致性、`ExclusiveWrite` 的不覆盖与整文件可见性 |

ghpipe 仓库自身**不是**模板仓库：不再有「clone 模板仓库获得工具源码」的用法，工具只以二进制形式进入目标项目。这里的三平台矩阵比两个参考实现都严——它们只在 Ubuntu 上跑测试、Windows/macOS 只有构建产物（§4.5.1）。

### 12.5 端到端自动化：人工触点清单与消除方案

目标：**主会话能连续把任务跑到 done，人只在真正需要人判断的地方出现。** 现状与目标逐条对照：

| 触点 | 现状 | 目标形态 |
|---|---|---|
| 首次接入：创建/安装 GitHub App | 必须浏览器（GitHub 无 API，§11.1.1） | 不变，但压成**一次会话两次点击**，其余由 `ghpipe init app` 自动完成 |
| 签发角色会话 | 每个 PR/SHA 人工一条命令、手工抄 SHA | `ghpipe execution issue --auto-bind`：一条命令自动解析 PR 与 head 并签发；输出可直接转交的交接片段。团队场景可选 GitHub Environment 审批的集中签发（一次点击） |
| 选择下一个任务 | 人工翻 Issue 列表 | `ghpipe task next --service` 只读给出候选与排序（依赖已 done、批次已放行、无阻塞、无冲突 PR、优先级/里程碑），主会话据此 `task claim` |
| 等待 CI / Review | 人工轮询 | 主会话按 `status` 的 `next_actions` 轮询，配合有退避的等待；`status` 输出就是等待理由 |
| 元数据同步 | 可能人工改标签 | workflow 自动（事件 + schedule 17/47 + `concurrency: queue: max`）；主会话只回读 `metadata.changes` |
| 合并后收尾 | 人工删分支 | merge 自动触发 cleanup + 审计声明；失败用 `cleanup --pr` 恢复 |
| 发布/放行 | 人工跑 `publish --apply` / `release --apply` | 仍由 Owner 会话执行，但可被 label 或 `workflow_dispatch` 触发，Owner 只需触发/批准 |
| 项目资源升级 | 人工比对 diff | 每日 workflow 跑 `ghpipe init resources --check`（只读退出码）→ 有差异**自动开 PR** → 人只审 PR 与合并 |
| CLI / 技能升级 | 手动下载 | `ghpipe update`（识别渠道 → 更新二进制 → 同步技能入口 → 报告项目资源差异，§13.6） |
| npm 发布 | 人工 OTP | npm Trusted Publishing（OIDC，§13.3） |
| 会话/密钥轮转 | 到期才发现 | 到期前提醒（`execution log` + schedule workflow 通知 Owner）+ `execution rotate` 一条命令轮换 |

为减少"每次都要做决定"，约定一组默认值（都可用参数覆盖）：合并一律 squash、返修上限 3 轮、merge 后自动 cleanup、技能布局默认 `separate`、App 安装范围默认"仅当前仓库"、连通性/命令超时默认 60s、loopback 回调默认 10 分钟、`--check` 类命令默认只读。

---

## 13. 分发与发布

### 13.1 产物命名

```
ghpipe_v0.1.0_darwin_amd64.tar.gz     # macOS Intel
ghpipe_v0.1.0_darwin_arm64.tar.gz     # macOS Apple Silicon
ghpipe_v0.1.0_linux_amd64.tar.gz
ghpipe_v0.1.0_linux_arm64.tar.gz
ghpipe_v0.1.0_windows_amd64.zip       # Windows 用 zip（解包不依赖 tar）
ghpipe_v0.1.0_windows_arm64.zip
ghpipe_v0.1.0_linux_386.tar.gz        # 可选：32 位 x86
ghpipe_v0.1.0_linux_armv7.tar.gz      # 可选：32 位 ARM
ghpipe_v0.1.0_windows_386.zip         # 可选
checksums.txt                         # 所有资产的 sha256
```

tag 使用 `vX.Y.Z`；初始版本见 §17 D7。

必需目标是 6 个（三平台 × amd64/arm64），全部 `CGO_ENABLED=0` 静态编译；32 位目标是可选产物，发布失败不阻塞必需目标。

### 13.2 npm 包装层（沿用 cc-connect 结构，补齐校验与容错）

```text
npm/
├── package.json     # bin: ghpipe → run.js；scripts.postinstall → install.js；files: [install.js, run.js, README.md]
├── install.js       # 按平台下载 + sha256 校验 + 解包（tar.gz / zip）+ chmod + 去 macOS quarantine
├── run.js           # 版本检查（缺失/过期/损坏才重装，绝不降级），转发 argv 与退出码
└── README.md
```

相对 cc-connect 的加强项：

| 项 | 设计 |
|---|---|
| 完整性校验 | 下载 `checksums.txt` 并校验资产 sha256，不匹配即失败（cc-connect 未校验） |
| 镜像回退 | 依次尝试 `GHPIPE_DOWNLOAD_BASE`（自定义镜像）→ GitHub Release；全部失败给出人工下载指引 |
| 离线/受限安装 | `GHPIPE_SKIP_DOWNLOAD=1` 时跳过下载，允许打包机预置二进制；`run.js` 首次运行再尝试补装 |
| postinstall 行为 | 下载失败**不**让 `npm install` 失败（除非 `GHPIPE_STRICT_INSTALL=1`），把失败推迟到首次运行并给出明确原因；避免在受限网络下把整个依赖树装挂 |
| 平台声明 | `package.json` 显式声明 `os: [darwin, linux, win32]` 与 `cpu: [x64, arm64]`（可选 `riscv64`），让 npm 在不支持的平台上直接拒绝安装而不是装完才发现；`process.arch` 映射 `x64 → amd64`、`arm64 → arm64` |
| Windows 细节 | 可执行文件名为 `ghpipe.exe`；不做 `chmod`；解包优先 `tar -xf`（Windows 10+ 自带 bsdtar），失败回退 `[System.IO.Compression.ZipFile]::ExtractToDirectory` 与 PowerShell `Expand-Archive`；下载用 `--ssl-revoke-best-effort` 规避 Windows Schannel 离线时的证书撤销检查失败；自更新留下的 `ghpipe.exe.old` 在启动时自动恢复 |
| macOS 细节 | 解包后 `xattr -d com.apple.quarantine`（失败即忽略）；同时保留未签名二进制在 Gatekeeper 下的说明 |
| `bin` 形态 | 与 larksuite/cli 一致，`bin` 指向 `run.js` 而不是直接指向二进制：这样可以先做版本检查、必要时补装、再转发 argv 与退出码 |
| 版本协议 | `run.js` 解析 `ghpipe --version` 输出，**必须**形如 `ghpipe <semver>`（这是包装层与二进制的接口契约，需在二进制侧加测试） |
| 不降级 | 已装二进制版本 ≥ 期望版本时保留（沿用 cc-connect 的 `isNewerOrEqual`） |

### 13.3 发布流水线

`.github/workflows/release.yml`，tag `v*` 触发：

1. `go test ./...`
2. 交叉编译矩阵（`CGO_ENABLED=0`，注入 `-ldflags "-X .../version.Version=<tag>"`）：darwin/linux/windows × amd64/arm64 为必需，32 位目标可选
3. 打包 + 生成 `checksums.txt`
4. 创建 GitHub Release 并上传资产
5. `npm publish --provenance --access public`（npm 侧版本号与 tag 一致，发布前做一致性校验）

工具选择：优先 **GoReleaser**（矩阵、校验和、changelog、可选 brew tap 都是既有能力）；若希望零额外工具，用 `Makefile` + `gh release`（与 cc-connect 同构）。两者都可行，取 GoReleaser 为默认，理由是可复现与少写脚本。

发布认证：**使用 npm Trusted Publishing（OIDC）而不是长期 token**。npm 现在对「直接发布」要求浏览器强认证，且官方正在收紧可绕过 2FA 的自动化 token；OIDC 方式由 release workflow 通过 `id-token: write` 换取一次性发布凭据，既不需要在仓库里存放 `NPM_TOKEN`，也天然带 provenance。仓库侧需要先在 npm 包设置里登记 trusted publisher（GitHub 仓库 + workflow 文件名）。

### 13.4 其它安装通道（非目标，但成本低）

```bash
go install github.com/ghpipe/ghpipe/cmd/ghpipe@latest   # 免费获得
brew install ghpipe/tap/ghpipe                          # 可选，GoReleaser 直接支持
```

### 13.5 包名与作用域

| 名称 | 取值 | 说明 |
|---|---|---|
| npm 包名 | `@ghpipe/cli` | 作用域由 npm 组织 `ghpipe` 持有；无作用域的 `ghpipe` 会被 registry 的名称相似度规则拒绝（与既有包 `unpipe` 冲突），组织作用域不受该限制 |
| 可执行命令 | `ghpipe` | 由包内 `bin` 字段决定，与包名相互独立 |
| GitHub 仓库 | `ghpipe/ghpipe` | 与包名解耦；`org/repo` 同名是本项目采用的命名 |
| 本地目录 | `~/code/ghpipe` | 仅为本地路径，不参与分发 |

**发布时机：首个功能版本（`0.1.0`）随 release 流水线发布，不发布占位版本**——避免在 npm 包页面与 git 历史里留下过程性内容。

发布前核对（由 release 流水线或 Owner 执行）：`npm whoami` 与 `@ghpipe` 组织权限可用 → 包内版本号与 tag 一致 → `npm pack --dry-run` 只含预期文件 → 发布后 `npm view @ghpipe/cli version` 能读到新版本 → 在干净前缀执行一次 `npm install -g @ghpipe/cli` 并确认 `ghpipe --version`。首次发布新包时 registry 有数分钟读缓存延迟，读到 404 不要立即重发。

### 13.6 自更新与 `ghpipe update`

参考实现里有 `lark-cli update`，一条命令完成「检测安装方式 → 更新二进制 → 同步技能 → 写状态 → 提示」。ghpipe 采用同一形态：

```text
ghpipe update [--check] [--force] [--layout separate|suite] [--json]
```

| 步骤 | 行为 |
|---|---|
| 1. 检测安装方式 | 由当前可执行文件路径 + PATH 判定：`npm` → `InstallNpm`；`pnpm` → `InstallPnpm`；位于 `go install` 输出目录 → `GoInstall`；其余为 `Manual`（脚本或手动下载的静态二进制） |
| 2. 查询最新版本 | npm registry 的 `@ghpipe/cli` dist-tags（`latest`）；`--json` 输出 `current`/`latest`/`update_available`；`--check` 到此为止，不写任何东西 |
| 3. 更新二进制 | npm/pnpm：`npm install -g @ghpipe/cli@<version>` / `pnpm add -g @ghpipe/cli@<version>`；GoInstall：提示 `go install github.com/ghpipe/ghpipe/cmd/ghpipe@latest`；Manual：下载对应平台资产、校验 `checksums.txt`、原子替换（Windows 先把运行中的 `ghpipe.exe` 改名为 `.old` 再替换，失败可回滚，包装层启动时可恢复 `.old`） |
| 4. 同步技能入口 | 复用与 `ghpipe skills sync` 相同的实现（链接优先、拷贝兜底），刷新 `.ghpipe/state/skills-state.json`；除非 `--force`，状态已记录同版本则跳过 |
| 5. 报告项目资源差异 | **只报告不修改**：输出 `resources: {cli_version, resource_version, needs_upgrade}` 并提示 `ghpipe init resources`（原因见下表） |
| 6. 退出码 | 已最新或更新成功 = 0；`--check` 发现可更新 = 1（便于脚本判断）；更新失败 = 1；结果不确定 = 2 |

与参考实现唯一的实质差异来自**正本位置不同**：

| 维度 | 参考实现（larksuite/cli） | ghpipe |
|---|---|---|
| 技能正本 | `~/.agents/skills/<name>`，在宿主目录，属工具自管 | `.ghpipe/skills/<name>`，在仓库内，是 git 跟踪的项目资产 |
| `update` 能否顺手同步技能 | 能，直接重新 add | 能：同步工具侧入口（`.agents/skills`、用户级路由技能）不改仓库 |
| `update` 能否顺手升级项目资源文件 | 不涉及 | **不能**：改 `.ghpipe/AGENTS.md`、`skills/`、`references/`、`templates/` 属于仓库变更，必须走开发分支 + PR + 独立验收；`update` 只报告差异并给出命令 |

其他一致性细节：Windows 上人类输出用 ASCII 回退（`[OK]`、`->`）而不是 `✓`/`→`；`--check` 与 `--layout` 互斥；CI 环境与 dev 构建不做更新检查，`GHPIPE_NO_UPDATE_CHECK=1` 可关闭；版本比较统一剥掉前导 `v`（tag 是 `v0.1.0`、npm 是 `0.1.0`）。

npm 包装层的 `run.js` 与 `ghpipe update` 是同一套保障的两个入口：包装层覆盖「npm 安装的机器」（版本不符时自动补装），`ghpipe update` 覆盖「手动/脚本安装的机器」，两者共用同一版本协议（`ghpipe --version` → `ghpipe <semver>`）与「不降级」规则。

---

## 14. 测试策略

### 14.1 分层

| 层 | 手段 | 覆盖 |
|---|---|---|
| 纯函数单元测试 | `lifecycle`、`attribution`、`policy` 判定、`publish` 校验、配置校验、会话校验 | 无 I/O，用例可穷举 |
| 传输协议测试 | `httptest.Server` 构造分页/异常响应 | Link 分页、重复页、total_count 不足、GraphQL cursor、rollup SHA 不匹配、错误分类 |
| Git 集成测试 | 临时仓库 + 真实 `git` | 分支守卫、署名提交、push refspec、cleanup 的锚点与保护、attribution 历史核验 |
| CLI 端到端测试 | 编译二进制，跑真实进程（`GHPIPE_TEST_BINARY`） | 退出码、`--json` 信封、参数位置自由、stdout/stderr 分离 |
| 分发测试 | 假 Release + `npm pack` | install.js 下载/校验/解包、run.js 版本协议与不降级 |
| Skill 与 App 接入测试 | 临时 `HOME` + 临时项目 + `httptest` 假 GitHub | 链接创建/冲突/悬空、复写与自定义检测、三方差异、manifest 回调与 conversion、安装范围校验 |
| 文档/资源测试 | 哈希与结构校验 | 内嵌资源齐全、Skill frontmatter 合法、相对链接可达、`--version` 格式 |
| 跨平台测试 | 三平台 CI + 构建标签隔离的平台专属用例 | 文件锁互斥、原子替换占用重试、密钥保护检查（0600 vs user_profile_acl）、符号链接与拷贝回退、CRLF 工作区下的资源哈希一致、路径分隔符处理 |

### 14.2 必须保留的回归清单

来自现实现的 363 个用例，重写后至少覆盖这些行为（每条一个用例，命名保持可读）：

1. 未携带会话/主体的写命令被拒绝；identity 文件不能授权。
2. 角色能力越界被拒绝（developer 不能 review/merge；reviewer 不能 commit/push）。
3. 会话绑定不匹配（root/cwd/origin/分支/PR/SHA）逐一被拒绝；linked worktree 被拒绝。
4. 同一 nonce 的重复写不产生第二次写；pending 存在时新写被拒绝；明确拒绝清回执；未知保持 pending。
5. `journal resolve` 只在正向证据成立时把 pending 置为 reconciled，且不重放。
6. 分页重复页、满页无 next、total_count 不足、后页失败都判为不完整读取。
7. GraphQL cursor 重复/缺失、rollup SHA 不匹配判为 incomplete/contract 错误。
8. 同名 check-run 与 commit status 并存时两者必须满足；来源不符判 failed。
9. merge gate：APPROVED + CLEAN 通过；BLOCKED 仅在可证明 PR-only writer 时 eligible；bypass/未知规则一律拒绝；非 squash/`--admin`/`--auto` 拒绝。
10. Review 必须绑定当前 head，写后 head 变化判 unknown。
11. 已合并 PR 重复 merge 只确认成功并进入 cleanup；cleanup 保护脏工作区/其他 worktree/无关提交。
12. 元数据：唯一 `Closes #N`、正文第二处关闭引用阻断、PR milestone 置空、标签继承、重复 DELETE 404 的幂等判定。
13. 生命周期投影的全部阶段与 closing 子事实。
14. 质量门禁：bootstrap 不能自我降级、业务变更必须有真实 test 命令、符号链接/子模块拒绝、脏检出拒绝、环境变量清洗。
15. 发布幂等：既有对象内容不一致即拒绝；已 released 批次不新增切片；重放不重复创建。
16. 署名：提交 Author/Committer/trailers 精确匹配；hook 改写被检测；merge/rebase 中拒绝普通提交；`attribution.required` 的边界核验。
17. 配置：非法命令键、shell 字符串命令、cwd 越界、不同 App 复用同一 ID、改绑需 `--rebind`。
18. 凭据：注册表/私钥在检出内被拒绝；保护级别校验（POSIX 0600 / Windows user_profile_acl）；过期未刷新即阻断；续签锁与原子写。
19. 资源升级：预览后内容变化即拒绝应用；未识别内容保留；退休仅限受管且未修改文件。
20. 状态：`--offline` 不读凭据；blockers 严重度合并规则；done 的 next_actions 为空。
21. Skill 分发：sync 幂等（重复执行不产生差异）；默认建相对链接、链接不可用时转拷贝；不是 ghpipe 写的同名路径默认拒绝覆盖并报告 `foreign`；路径解析越界拒绝；`skills status` 能发现 `missing`/`modified`/`foreign`/`stale`。
22. Skill 复写：修改受管 Skill 后被识别为 modified，升级不覆盖而是产出 `.new` 与三方差异；自定义 Skill 永不被触碰；本地层同名时入口内容切换为本地层，且同一发现路径下不出现两个同名技能。
23. 官方约定符合性：`SKILL.md` frontmatter 的 `name`/`description` 合法；每个技能目录含可解析的 `agents/openai.yaml`；description 单行、≤80 字符、含触发词；用户级只有 `ghpipe` 一条路由技能；`skills sync` 生成的链接位于 git root 的 `.agents/skills`，且能从子目录向上发现。
24. App 接入：manifest `state` 不匹配/重复回调拒绝；conversion 响应非法时不写本地状态；安装范围非当前仓库时 `doctor` 报错或 warn；路径 C（用户 token）不声称 App 级隔离。
25. AGENTS.md 受管块：注入与升级保留用户自有内容；块外内容与用户新增章节在升级后逐字不变；受管块本身保持 2–3 行，不把机制文档写进 `AGENTS.md`。
26. 跨平台：Windows 上无 `flock` 时锁仍互斥、目标被占用时原子替换按退避重试并最终明确失败、`CheckSecret` 返回 `user_profile_acl` 而非假装 0600；符号链接不可用时 `skills sync` 自动转拷贝且 `skills status` 报告 `copied`，可链接时报告 `linked`。
27. 跨平台：CRLF 检出的资源哈希与 LF 一致（`NormalizeText`），复写/退休判定不因换行符误判；仅大小写不同的同名技能被拒绝。
28. 技能读取：`skills read` 在链接、拷贝、纯内嵌三种状态下都返回与二进制同版本的内容，并报告 `source_layer`；项目复写后立刻生效。
29. 漂移提示：本地 `skills-state.json` 记录版本与二进制不一致时输出一行修复建议，且 CI、dev 构建与显式环境变量下静默；提示本身零网络、零子进程。
30. 覆盖语义：`linked`/`copied` 入口被同步直接覆盖刷新；把拷贝内容改掉后 `skills status` 报 `modified` 且默认不覆盖；路径不属于 ghpipe 时报 `foreign`；源内容或 CLI 版本变化后报 `stale` 而不是静默过期；技能退出受管集合时按状态记录决定删除或保留；`--layout suite` 只产生一个入口技能。
31. 自更新：安装方式识别（npm / pnpm / go install / 手动）各自走对应分支；`--check` 不产生任何写入且与 `--layout` 互斥；`--force` 在已最新时仍重新安装；升级二进制后自动同步技能入口并刷新状态；项目资源差异只报告不修改（断言 `.ghpipe/` 内文件在 `update` 后逐字节不变）；Windows 替换失败可从 `.old` 回滚。

### 14.3 不承诺的事项（必须在文档与 doctor 输出中保持）

离线 mock、协议探针、配置成功都不等于真实 App 权限、组织 SSO、真实分支保护拦截或业务验收。工具自身 CI 只证明工具行为：三平台矩阵证明命令行为与文件系统行为在 macOS/Linux/Windows 上一致，不证明每个平台上的真实 App 权限或业务 CI 已验收；32 位目标只有构建产物，不在测试矩阵内。

### 14.4 测试与验收的硬规则（来自第一次自举实践）

第一次按本设计跑「独立开发 → 独立验收 → 合并」时踩到的三个坑，已经固化成规则：

| 规则 | 原因 | 落地方式 |
|---|---|---|
| **测试不得绑定监听端口** | 开发与验收 agent 都跑在沙箱里，`httptest.NewServer`/`net.Listen` 会因 `bind: operation not permitted` 直接 panic；在沙箱外跑通过不代表可用 | HTTP 层测试用注入式假 `RoundTripper`（或 `httptest.NewRecorder` + 直接调用 handler）；CI 与本地默认都不需要网络或端口 |
| **验证测试必须用 `-count=1`（或等价禁用缓存）** | Go 会缓存测试结果：一次沙箱外的成功会让沙箱内显示 `ok (cached)`，把真实失败掩盖掉 | `quality check --run-tests` 执行 `commands.test` 时，若命令是 go test 一律要求带 `-count=1`；文档与 Skill 的自检清单同样带该参数 |
| **交接信息写在 Issue/PR 上，不依赖 agent 间消息** | 实践中出现过派发消息未送达子 agent 的情况（子 agent 只拿到环境、没有任务正文） | 任务范围、变更要求、验收标准一律落 GitHub（Issue 正文 + 评论），任务正文是对子 agent 的**权威来源**；派发指令里要求子 agent 先读 Issue 及评论，做到「消息丢了也能从账本恢复」。这是对原生子 agent 派发（§6.8）的加固，不是替代 |
| **平台差异只能由多平台 CI 判定，本地沙箱与交叉编译都不算** | 首次运行三平台矩阵就在 `windows-latest` 抓到一个本地与交叉编译都发现不了的缺陷（`filepath.IsAbs("/tmp")` 在 Windows 上为 false，导致 `commands.*.cwd` 可指向项目外） | `.github/workflows/ci.yml` 的 `test` job 覆盖 ubuntu/macos/windows；分支推送即触发（我们直接合并分支、不开 PR），合并前必须三平台全绿；本地沙箱只作为快速反馈 |

补充一条实践结论（不改变设计，只是记录）：当执行 agent 的沙箱把 `.git` 挂成只读、且无网络时，它只能产出**工作区改动**；此时由调度者在核对产出后代理提交与推送，并在提交信息里注明产出者与代理原因。这与「提交必须由 Developer 完成」的默认约定并不冲突——约定的是**内容责任**，而不是磁盘权限。

---

## 15. 实施计划

每个阶段结束都有「可运行 + 可测 + 有验收条件」的产出，不接受「先写全部代码再调试」。

| 阶段 | 交付 | 验收条件 |
|---|---|---|
| P0 骨架 | `go.mod`、命令树、`--json` 信封、退出码、版本与兼容性、`hostfs`（锁/原子替换/密钥保护/链接回退/文本规范化）、`project`/`credentials`/`trust`/`journal`、`gitx` 基础、`ghttp` 传输与分页契约 | 纯本地命令在 macOS、Linux、Windows 三平台均可运行；分页与错误分类用例全绿；跨平台用例通过；无法执行任何远端写 |
| P1 只读面 | `inspect`、`status`、`doctor --offline/--connectivity`、`metadata`（只读）、`quality check` | 对真实仓库只读跑通（至少一个 Windows 检出与一个 macOS/Linux 检出）；不产生任何写；`--offline` 不读凭据 |
| P2 开发面 | `identity`、`commit`、`push`、`pr create/edit/comment`、`journal`、attribution 核验、任务绑定 | 在一次真实交付中由 Developer 会话开出 PR；journal 覆盖未知写用例 |
| P3 验收与合并 | `pr review`、merge gate、`pr merge`、`cleanup` + 结算声明、`task release` | 真实仓库完成一次独立验收 + squash 合并 + 清理 + done |
| P4 接入面 | `init repo/app/checks/metadata/resources/seed`、`init app` 全流程（manifest + 回调 + 安装校验 + `repos`）、`auth login`（路径 C）、`execution` 签发链路、资源升级/退休 | 空仓库 bootstrap → 初始化 PR → strict 切换全流程；一条 `init app` 在只点两次的情况下完成创建安装绑定 |
| P4.5 Skill 体验 | `skills list/new/clone/diff/reset/sync/status`、工具路径表、AGENTS.md 受管块、复写与三方差异 | 在 codex + pi + zcode（本机已装）验证发现与生效；复写官方 Skill 后在升级中不丢失且能合并 |
| P5 发布与自动化 | `publish`/`release`、三个工作流模板、automation 会话 | 批次发布 → 放行 → 自动化同步标签收敛 |
| P6 分发与文档 | npm 包装层、GoReleaser、release 流水线（三平台 × amd64/arm64，Windows 用 zip）、`ghpipe update`（渠道识别 + `--check` + 技能同步 + 项目资源只报告）、Skills/references 重写、`docs/cli.md` | `npm i -g @ghpipe/cli` 后在 macOS、Linux、Windows 各一台干净机器完成 P2–P3 全流程；手动安装的机器用 `ghpipe update --check` 与 `ghpipe update` 各验证一次 |

停止条件：任一阶段出现「无法用真实 GitHub 行为证明」的项，停下来把该项移出承诺范围或要求用户提供验证环境，不用本地模拟替代。

**进展（2026-09-13）**

- **P0 骨架已合并**（`97b0bcc`）：`go.mod`、命令树与统一 `--json` 信封、退出码契约（0/1/2/3）、`internal/hostfs`（文件锁双实现、原子替换、CRLF 规范化、密钥保护检查）、`internal/project`（配置发现与校验）、`version` 与 `inspect` 两个命令；`go vet` / `go test` 通过，六目标交叉编译通过。
- **P1a 已合并**（`4d97521`，Issue #1）：`internal/github` 的进程内传输、REST 分页契约、GraphQL 连接契约、错误分类与端点脱敏。流程按本设计执行：独立 Developer agent 实现（`f9a4696`）→ 变更要求（测试不得依赖监听端口）后修复（`d7b1813`）→ 另起独立 Reviewer agent 在固定 SHA 验收（变异测试 11 处注入缺陷全部被捕获，结论 APPROVE）→ 调度者 squash 合并并清理分支。
- Reviewer 的非阻断发现已登记为 **Issue #2**（bug）：Link 头按逗号切分会在 URL 含逗号时静默丢页；跨主机重定向被拒时的错误分类过于笼统。
- **P1b-1 已合并**（`5e5c04c`，Issue #3）：`internal/gitx`（只读 git 查询，Runner 带 context）+ `internal/lifecycle`（纯阶段投影，`Plan` 返回每个对象的阶段）。同样是独立开发（两轮：初版 + 变更要求）→ 独立 Reviewer 固定 SHA 验收（26 处变异全部被测试捕获，APPROVE）→ 调度者合并清理。
- **仓库自带 CI 已落地**：`.github/workflows/ci.yml` —— 三平台测试矩阵（ubuntu/macos/windows，`go vet` + `go test -count=1`）与六目标交叉编译矩阵。这就是「多平台编译与测试」的默认验证方式，本地沙箱只作为快速反馈。
- **CI 首跑即抓到真缺陷**（Issue #4，已修复并合并 `76af541`）：`commands.*.cwd` 用 `filepath.IsAbs` 判断，Windows 上 `/tmp`、`\foo` 只是 rooted 而非 absolute，可通过校验并解析到项目外。修复方式：新增 `internal/project.IsProjectRelative`（跨平台判定，拒绝空串/绝对/`/`与`\` 开头/UNC/盘符/`..` 段），`cwd` 与 `ci.workflow_path` 复用；回归用例在 Linux 上同样能抓住该缺陷。平台结论由 CI run `34754753405` 提供（三平台测试全绿），独立 Reviewer 在同 SHA 验收通过。
- 下一片 **P1b-2**：只读面命令——`status`（需要 `metadata` 事实收集 + `checks` 事实 + `policy` 门禁）、`doctor --offline`、`metadata`（只读）、`quality check`。

---

## 16. 风险

| 风险 | 影响 | 应对 |
|---|---|---|
| 自研传输与分页的边界情况 | 读取不完整被误当完整 | 分页契约作为独立包 + 专项用例；不完整读取一律 unknown |
| 没有 gh 后失去命令行便利 | 用户/Agent 习惯用 `gh pr checks` 等 | `ghpipe api GET` 保留任意只读 REST；文档明确 gh 仍可人工使用，但不参与门禁 |
| 生成的 workflow 需要下载二进制 | CI 首次运行多一步网络依赖 | 固定版本 + checksum + npm/Release 双通道；失败即 job 失败并给出可操作提示 |
| 目标项目 CI 依赖 npm | 无 npm 的项目也要装 node | 备选 B（Release 直连 + curl + sha256）作为无 npm 环境的文档化路径 |
| Windows 语义差异（无 `flock`、无 POSIX mode、符号链接需开发者模式、CRLF 检出） | 若不处理会出现「锁失效 / 权限谎报 / Skills 装不上 / 资源被误判为已修改」 | 全部差异收敛到 `hostfs` 抽象并在三平台 CI 覆盖；符号链接失败自动拷贝回退；哈希统一按 LF 规范化；文档与 `doctor` 如实报告保护级别（§4.5） |
| 三平台行为漂移（例：路径、大小写、控制台编码） | 同一命令在不同平台表现不一致，难以复现 | 三平台测试矩阵 + 平台专属用例；仓库相对路径统一用 `path`，本地 FS 用 `filepath`；`--json` 输出恒定 UTF-8 无转义 |
| GitHub 强制 App 创建/安装必须走浏览器 | 无法做到「完全在 CLI 内」 | 把两次点击合并为一次会话，其余全自动（§11.1.2），并提供 `--no-browser`/`--paste`/SSH 转发与路径 B/C 备选；在文档与命令输出里如实说明这一约束，不承诺做不到的事 |
| 工具 Skill 约定变动频繁 | `skills sync` 可能写错目录或被工具升级后失效 | 路径表外置可配置（`tools.json`）；`skills status` 与 `doctor` 报告漂移；`agents-md` 作为不依赖发现约定的兜底 |
| 项目复写官方 Skill 后与新版脱节 | 复写的 Skill 长期停留在旧流程 | managed/modified/custom 三态显式可见；升级产出 `.new` 与三方差异；`skills diff`/`reset` 提供收敛路径 |
| 安装范围调整依赖 classic PAT | 与「最小权限」目标冲突 | 默认走浏览器一次性调整；`--token-stdin` 明确要求用后即弃、不落盘，并在输出中标注该操作使用了宽权限凭据 |
| 与 aipipe 并存造成混淆 | 用户误把两套配置混用 | 目录与命令完全分离；文档与 `inspect` 明确不读 `.aipipe/` |
| 内嵌资源与二进制版本漂移 | Skill 与命令不匹配 | `manifest.json` + 资源 digest CI 校验；资源与二进制同版本发布 |
| 会话/回执机制偏重 | 日常操作步骤多 | 通过 identity 默认路径、统一 `--json`、`journal` 单一入口降低摩擦；不为省步骤牺牲「未知写不重放」 |

---

## 17. 决策清单与状态

### 17.1 已定（你已明确确认）

| # | 决策 | 结论 |
|---|---|---|
| D5 | 平台范围 | macOS / Linux / Windows × amd64、arm64 为必需目标；32 位目标为可选产物 |
| D7 | 包名与发布时机 | npm 包名 `@ghpipe/cli`（组织 `ghpipe` 已建）；**不发布占位版本**，首个功能版本随 release 流水线发布 |
| D13 / D16 | 技能分发策略 | 照 larksuite/cli：内嵌内容 + `skills read` 兜底；链接优先、拷贝兜底；更新=重新安装即覆盖 + 清理退出项 + 回读校验；用户级只放 1 条路由技能 |
| D20 | 部署形态 | 单机单用户，不使用独立容器/OS 用户；安全由「阻止误用与顺手提权 + 缩小泄露窗口（少落盘、短有效期）+ 权限天花板 + 账本可检测」承担 |
| D27 | 缺陷类型与回归要求 | 缺陷为一等任务类型；默认强制回归测试（`verify regression` 先红后绿）；`blocker`/`critical` 另需 PR 正文根因分析；偶发缺陷走显式例外 |
| D28 | 用户提交与分诊权限 | Issue Forms 收报告，默认 `ghpipe:triage`、未分诊不可领取；接受/拒绝由 Owner 执行；不改写用户原文 |
| D29 | 自动上报 | 默认关闭；需专用凭据 + `report preview` 可预览；只发白名单字段；失败不重试、不影响原命令退出码；默认中间态是"本地留痕 + 提示人工提交" |
| D30 | 公共上报 App | 首版不做 |

### 17.2 已定（按推荐执行）

以下决策已确认按推荐值执行；分组只用于说明"在哪个阶段开始生效"。

**A 组｜必须在 P0 之前定（影响代码骨架与对外契约）**

| # | 决策 | 推荐 |
|---|---|---|
| D1 | 命名与路径 | 二进制 `ghpipe`、项目目录 `.ghpipe/`、分支 `ghpipe/issue-N`、工作流 `.github/workflows/ghpipe-*.yml` |
| D2 | GitHub 访问方式 | 完全去掉 `gh` 子进程依赖，自研进程内传输；不引入 go-gh |
| D4 | 功能取舍 | 删除 `preferences`、`metadata.mode`、gh 透传、`scripts/*.py`；保留批次发布/Skills；旧 `error_reporting` 由 §10.12 的 opt-in 上报取代 |
| D6 | 退出码与结果契约 | 0 成功 / 1 确定失败 / 2 结果未知 / 3 用法与前置错误；所有命令支持 `--json` 信封 |
| D8 | 会话密钥算法 | 会话与信任根用 Ed25519；GitHub App JWT 用 RSA/RS256 |
| D9 | 配置文件名 | `.ghpipe/config.json` |
| D19 | 输出语言 | 命令、标志、JSON 字段、错误短句用英文；Skills 与文档用中文 |
| D24 | 目录覆盖边界 | `GHPIPE_CONFIG_DIR` 只覆盖非安全状态；信任根与 Owner 材料固定取 OS 用户配置目录 |

**B 组｜对应功能开工前定**

| # | 决策 | 推荐 |
|---|---|---|
| D11 | App 接入路径 | 路径 A（`init app` 一条命令驱动）为默认，路径 B（复用已有 App）/ C（用户 token）为备选 |
| D12 | 安装范围默认 | 仅当前开发仓库；`all` 允许但 `doctor` warn |
| D14 | 项目复写落点 | 就地复写 `.ghpipe/skills/`（manifest 哈希判定 modified，升级产出 `.new`）+ `.ghpipe/skills.local/` 本地层 |
| D15 | 隐式调用策略 | 角色类技能允许隐式；Owner 类（init/publish/release）设为 `false`，必须显式调用 |
| D23 | 硬链接拒绝范围 | 只对凭据/私钥拒绝多链接文件；仓库资源只拒符号链接 |
| D25 | 默认密钥保护 | **已定（简化）：不做加密、不做生物识别/硬件密钥**。凭据就是 0600 文件（Windows 用户配置目录 ACL），保护来自少落盘（Delivery token 默认 `--ephemeral`）、短有效期与可审计 |
| D26 | 文档阶段测试策略 | 默认照常跑 `commands.test`；可选 `docs_fast_path`（仍产生同名成功检查 + 真实文档检查） |

**C 组｜发版前定**

| # | 决策 | 推荐 |
|---|---|---|
| D3 | CI 获取二进制的方式 | 默认 npm 包装层固定版本；Release 直连作为无 npm 环境的备选 |
| D10 | 仓库许可证 | MIT（与参考实现一致）；需要你确认才创建 `LICENSE` |
| D17 | 默认技能布局 | `separate`（逐技能，便于显式调用）；`suite` 作为可选 |
| D18 | 自更新边界 | `ghpipe update` 更新二进制并同步技能入口，但不自动修改仓库内项目资源 |
| D21 | 签发自动化程度 | 首版：Owner 本地签发 + `--auto-bind`；集中签发服务后续可选 |
| D22 | 发布产物证明 | GitHub artifact attestations + npm provenance（OIDC）；cosign 可选 |

### 17.2.1 待你处理的两件事（不是决策）

1. **npm 撤包已完成**：`npm view @ghpipe/cli` 返回 404，包已撤下；作用域仍归组织 `ghpipe`，首个功能版本直接发 `0.1.0`。
2. **GitHub 仓库重建**：需要在网页删除 `ghpipe/ghpipe` 后重新创建，才能彻底清除强推后残留的悬空提交。重建后我把当前单提交历史重新推上去。

### 17.3 决策明细

| # | 决策 | 推荐 | 影响 |
|---|---|---|---|
| D1 | 命名与路径 | 二进制 `ghpipe`、目录 `.ghpipe/`、分支 `ghpipe/issue-N`、工作流 `ghpipe-*.yml` | 影响全部文档、Skill、模板与命令；不兼容 aipipe |
| D2 | GitHub 访问方式 | 完全去掉 `gh` 子进程依赖，自研进程内传输；**不引入 go-gh** | 去掉一个外部依赖；需自持分页/GraphQL 契约实现（已在 §7 细化） |
| D3 | CI 获取二进制的方式 | 默认 npm 包装层（`npx ghpipe@<固定版本>`），Release 直连作为无 npm 环境的备选 | 决定三个生成工作流的写法 |
| D4 | 功能取舍 | 删除 `error_reporting`、`preferences`、`metadata.mode`、gh 透传、`scripts/*.py`；保留批次发布、metadata 工作流、7 个 Skill | 减少配置与代码面；若你希望保留自动缺陷上报，需要单独设计授权（当前实现实际已不授予上传权限） |
| D5 | 平台范围 | **三平台必需：macOS / Linux / Windows × amd64、arm64**（共 6 个必需产物）；32 位目标（linux/386、linux/arm v7、windows/386）作为可选产物 | 决定发布矩阵、三平台 CI 与 `hostfs` 抽象范围；Windows 的锁、权限、符号链接、CRLF 差异必须按 §4.5 实现，不能用「暂缓」绕过 |
| D6 | 退出码与结果契约 | 0 成功 / 1 确定失败 / 2 结果未知 / 3 用法与前置错误；所有命令支持 `--json` 信封 | 与 aipipe 不同（旧：0/1/2）；机器解析更明确 |
| D7 | 版本与包名 | 起始版本 `0.1.0`；npm 包名 **`@ghpipe/cli`**（作用域由 npm 组织 `ghpipe` 持有，命令名仍为 `ghpipe`），不发布占位版本，首个功能版本随 release 流水线发布（§13.5） | 安装指令为 `npm install -g @ghpipe/cli`；影响 release 工作流的版本号与 trusted publisher 配置 |
| D8 | 会话密钥算法 | 会话与信任根用 Ed25519；GitHub App JWT 用 RSA/RS256（GitHub 强制） | 去掉 openssl 依赖；宿主需重新生成主体密钥（无历史兼容） |
| D9 | 配置文件名 | `.ghpipe/config.json`（替代 `project.json`） | 影响全部文档与命令示例 |
| D10 | 仓库许可证 | 待你指定（建议 MIT，与 cc-connect 一致） | 影响 `LICENSE` 与 npm 元数据 |
| D11 | App 接入体验 | 路径 A 为默认（一条 `ghpipe init app` 驱动 manifest + 安装 + 校验，一次浏览器会话两次点击）；路径 B（复用已有 App）、路径 C（用户 token，无 App）作为备选一并实现 | 接入方式与文档主线；路径 C 会改变隔离模型，需要 `doctor` 如实区分 |
| D12 | 安装范围默认值 | 推荐 `Only select repositories → 当前开发仓库`；`all` 允许但 `doctor` 给出 warn | 影响安装时的引导文案与校验强度 |
| D13 | Skill 暴露机制 | 照搬 larksuite/cli 策略：项目级 `.agents/skills` **链接优先、拷贝兜底**（覆盖式同步）+ 用户级**单条** `ghpipe` 路由技能 + root `AGENTS.md` 受管块兜底；路径表外置可配置；`init repo` 默认只做 `agents-md`，不擅自写 home 目录 | 决定 `skills sync` 默认放置方式、技能数量与上下文占用；不触碰用户已有的第三方链接（标为 `foreign` 只读观察） |
| D14 | 项目级复写的落点 | 就地复写 `.ghpipe/skills/`（用 manifest 哈希判定 modified，升级产出 `.new`），额外提供 `.ghpipe/skills.local/` 本地层 | 决定升级语义与冲突处理体验 |
| D15 | 隐式调用策略 | 角色类技能（develop/review/orchestrate/resume/plan）保持 `allow_implicit_invocation: true`；Owner 类（init/publish/release 相关）设为 `false`，必须显式 `$ghpipe-init` 等调用 | 影响 Agent 会不会「自作主张」进入初始化或发布流程；无论哪种，CLI 的会话能力判定依然是最终边界 |
| D16 | 技能主路径与更新语义 | 照搬 larksuite/cli 策略：内嵌内容 + `ghpipe skills read` 兜底；放置链接优先、拷贝兜底；更新=重新安装即覆盖 + 清理退出项 + 回读校验入口集合；漂移用状态文件 + 启动单行提示；可选 `--installer npx-skills` 委托生态安装器，但不作硬依赖 | 决定 Windows 与受限环境下的可用性保证、更新语义与「工具入口不要手改」的约束；与飞书的唯一适配是正本位置（项目内 `.ghpipe/skills/`），已记录在 §11.2.7 |
| D17 | 默认技能布局 | 默认 `separate`（逐技能，便于按场景显式调用）；提供 `--layout suite` 聚合为单一入口技能，用于技能数量多或上下文预算紧张的场景 | 影响技能清单占用的上下文（官方 2%/8000 字符限制）与 Windows 上需要落盘的文件数 |
| D18 | 自更新边界 | 新增 `ghpipe update`：识别安装方式 → 更新二进制 → 同步技能入口 → 报告项目资源差异；**不自动修改仓库内的项目资源**（`.ghpipe/` 下的资源属仓库变更，仍走开发分支 + PR + 独立验收）；`--check` 只读 | 决定一条命令能走多远：宿主级改动自动化，仓库级改动保持可评审；也是与 larksuite/cli 的差异点（它的技能在宿主目录，所以能顺手同步） |
| D19 | 输出语言 | 命令、标志、JSON 字段、错误短句用英文（与 git/gh/生态一致）；Skills、references、设计文档用中文（延续现状）；错误里的关键修复命令保持可直接复制 | 影响全部 CLI 文案与测试断言 |
| D20 | 部署形态与隔离强度 | **已定：单机单用户，不使用独立容器/OS 用户**。不以文件权限作为角色隔离手段；改为「阻止误用与顺手提权 + 缩小泄露窗口（少落盘、短有效期）+ GitHub 权限天花板 + 账本可检测」；`doctor` 如实报告 `mode: same_user` 与哪些凭据长期在盘上 | 决定安全承诺的边界：不承诺"阻止同用户进程读取凭据文件"，承诺"阻止误用/顺手提权、把凭据暴露面压到最小、让跨界使用可查" |
| D21 | 签发自动化程度 | 首版：Owner 本地签发 + `--auto-bind`（一条命令解析 PR/SHA）；集中式签发服务（GitHub Environment 审批触发）作为后续可选 | 决定每任务的人工步骤是"跑一条命令"还是"点一次审批"；也决定是否引入额外的签发服务组件 |
| D22 | 发布产物证明 | 启用 GitHub artifact attestations 与 npm provenance（OIDC，无需额外密钥）；cosign 签名作为可选 | 让"下载到的二进制是谁构建的"可验证，成本接近零 |
| D23 | 硬链接拒绝范围 | 只在**凭据/私钥**（宿主目录，不受内容寻址工具影响）拒绝多链接文件；仓库内资源只拒绝符号链接，不拒绝硬链接 | 参考实现出于防走私理由拒绝一切多链接文件，但会误伤 pnpm/nix 这类内容寻址布局；按用途区分可同时保住安全与兼容 |
| D24 | 目录覆盖范围 | `GHPIPE_CONFIG_DIR` 只覆盖非安全状态（`tools.json`、`skills-state.json`、缓存）；**信任根与 Owner 材料始终取 OS 用户配置目录**，不受环境变量影响 | 环境变量能改信任根等于把提权入口交给环境；这条边界必须写死 |
| D25 | 默认密钥保护方式 | **已定（简化）：不做口令加密、不接 Touch ID/Windows Hello/FIDO2**。凭据是普通 0600 文件（Windows 用户配置目录 ACL）；实际收益靠 Delivery token 默认 `--ephemeral` 不落盘、会话按任务短时有效、写操作单次、全程可审计 | 决定实现复杂度与用户操作成本：每次签发/验收不需要额外解锁动作；同时也明确"同用户进程可读凭据文件"这件事不靠加密掩盖 |
| D26 | 文档阶段的测试策略 | 默认：文档/方案 PR 也照常跑 `commands.test`（简单、诚实）；可选 `quality.docs_fast_path: true` 在"全部变更都是文档"时跳过大测试，但必须产生同名成功检查 + 跑真实文档检查（markdown 结构 + 相对链接） | 决定文档/方案阶段的合并成本；无论哪种都不允许"检查不产生"或"假检查" |
| D27 | 缺陷类型与回归要求 | 缺陷作为一等任务类型（`templates/bug.md` + `publish --bug-file` + `ghpipe:type/bug` 标签）；`quality.bug_requires_regression_test` 默认 `true`：缺陷 PR 必须包含能复现缺陷的回归测试，并由 `verify regression` 给出"先红后绿"证据；无法稳定复现的偶发缺陷走显式例外（需在 PR 正文声明 + Reviewer 结论） | 决定缺陷能不能被"改完就说好了"；也决定验收是否需要额外跑一次临时 worktree 验证 |
| D28 | 用户提交入口与分诊权限 | 用 GitHub Issue Forms 收用户报告（默认标签 `ghpipe:triage`），**未分诊不可领取**；接受/拒绝由 Owner（`publish` 能力）执行，只读检查对任何角色开放；分诊不改写用户原文，只在正文尾部追加受管结构化块 | 决定外部贡献者能否顺畅提缺陷，以及"谁能把报告变成任务"这个权限边界 |
| D29 | 自动上报默认状态与凭据 | 默认关闭；开启需 `reporting.destination` + **专用** `credential_ref`（仅对上报仓库有 Issues 写权限，绝不复用业务角色凭据）；发送前必须能 `report preview` 预览；失败不重试、不影响原命令退出码 | 决定"项目自动提 bug 到工具仓库"是 opt-in 的显式行为还是隐蔽上传；也决定隐私与信任边界 |
| D30 | 是否提供共享上报 App | 首版不做：使用方自带凭据最透明；将来若要让上报"零配置"，再评估一个公共上报 App（代价是隐私与信任由工具方集中承载，类似第三方 SaaS） | 决定工具方是否要运营一个跨仓库写入的服务身份 |

补充说明（不需要你决策，但需知情）：

- npm 组织 `ghpipe` 已创建（作用域可用于 `@ghpipe/cli`）；无作用域 `ghpipe` 受 registry 相似度规则限制，见 §13.5。
- 本仓库当前只有设计文档（`README.md`、`docs/design.md`、`.gitignore`），尚无实现代码。

---

## 18. 缺口清单与处置

对照官方 Skill 规范（§11.2.9）、参考实现（§11.2.7）、角色隔离要求（§6.6）与自动化目标（§12.5）逐项盘点，本轮发现并已写进设计或待实现的缺口：

| # | 缺口 | 影响 | 处置 |
|---|---|---|---|
| G1 | `name` 命名规则（≤64、小写字母数字连字符、无首尾/连续连字符、必须等于目录名）未落入设计 | 技能可能不符合 open agent skills 规范，被工具拒绝或匹配异常 | 已补 `skills lint` 校验与 `skills new`/`clone` 的合规生成（§11.2.9） |
| G2 | `description` 只有"前置触发词"建议，没有官方要求的"说明何时使用 + 关键词"，也没有触发回归 | description 写偏导致隐式触发失败 | 已补写作规则 + `resources/skills-triggers.json` 与 `skills lint --triggers`（§11.2.9） |
| G3 | `SKILL.md` 体量约束（<5000 tokens、正文 <500 行、引用一层深）缺失 | 激活技能时挤占上下文，违背渐进披露 | 已补写作规则与 lint 检查（§11.2.9） |
| G4 | `agents/openai.yaml` 只写了两个字段 | 桌面端选择器缺少显示名/描述/图标，体验不完整 | 已补 `interface` 全字段与 `policy`、`dependencies.tools`（§11.2.9） |
| G5 | 带 `scripts/` 的技能与"脚本不内嵌"冲突未定义 | Agent 读得到 `SKILL.md` 却跑不了脚本 | 已明确：带脚本的技能必须拷贝模式；`skills read` 对脚本返回未落盘提示（§11.2.9） |
| G6 | 没有官方规范的本地校验器 | 每次改技能只能靠人工检查 | 已补 `ghpipe skills lint`；文档给出用 `skills-ref validate` 复核的路径（§11.2.9） |
| G7 | 角色隔离只散落在会话校验里，没有成体系的"反越权"约定 | 容易把流程约定当成安全机制，或漏掉"同密钥两角色"这类情况 | 已补 §6.6：一主体一角色/一角色一密钥/凭据不互换/注册主体校验/可读性告警 + 宿主边界说明 |
| G8 | 没有"任务选择"能力，主会话只能人工翻列表 | 人工决策多、易漏依赖与阻塞 | 已补 `task next --service`（只读候选 + 默认排序规则 + 项目可覆盖的 `selection` 配置）（§10.10、§12.5） |
| G9 | 签发会话要人工抄 PR/SHA | 每任务一次低价值操作，且容易抄错 | 已补 `execution issue --auto-bind`（自动解析 PR 与 head）（§10.10、§12.5） |
| G10 | 元数据 workflow 没写并发与幂等约束 | 重复触发可能双写标签或互相打断 | 待落到模板：`concurrency: {group: ghpipe-metadata-<repo>, queue: max}` + 写前事实校验 + 写后回读（§12.1） |
| G11 | 没有限流/二级限流处理策略 | 大仓库分页或短时间多发请求会 403/429，被误判为权限问题 | 待实现：GET 类请求按 `Retry-After` 有限退避重试（默认最多 3 次），**写请求永不重试**，分类输出为 `rate_limited`（§7.1） |
| G12 | 项目资源升级仍需人工比对 | 工具发版后项目长期落后 | 待实现：每日 workflow 跑 `init resources --check`，有差异自动开 PR，人只审 PR（§12.5） |
| G13 | 没有签发/使用审计与到期提醒 | 会话轮转靠记忆，过期才发现 | 已补命令 `execution log` / `execution rotate`，配 schedule 提醒 Owner（§12.5） |
| G14 | `npm-wrapper` 的 CI 没覆盖 Windows zip 路径 | Windows 安装问题要到用户那里才发现 | 待补：`npm-wrapper` job 增加 windows-latest 分支，验证 `.zip` + `ghpipe.exe` + shim（§12.4） |
| G15 | 发布产物没有来源证明 | 无法验证下载到的二进制是谁构建的 | 待实现：GitHub artifact attestations + npm provenance（D22） |
| G16 | `docs/cli.md` 手写容易漂移 | 文档与命令面不一致 | 待实现：由命令树/标志定义生成，CI 校验生成结果无差异（§14.1 `resource-digest` 同类任务） |
| G17 | 子进程输出与超时没有上限 | 长输出淹没上下文、卡死任务 | 待实现：`run` 与 git/gh 子进程统一超时 + 输出上限（超出截断并标注），关键命令保留完整输出到 state 文件 |
| G18 | 主会话的等待语义未定义 | 容易出现忙轮询或无限等待 | 已补：以 `status.next_actions` 为等待理由，采用有退避的轮询；单轮等待上限与超时在 Skill 中写明（§10.10） |
| G19 | 没有跨平台路径策略 | Windows 上的 UNC/设备命名空间/保留设备名可能让"项目外"校验被绕过 | 已补 `hostfs.ValidateInputPath`（拒绝清单与参考实现一致）（§4.3、§4.5.1） |
| G20 | 只有"覆盖式原子写"一种语义 | journal pending、App 私钥、主体注册表被覆盖或半写会破坏"未知写不重放"与信任链 | 已补 `ExclusiveWrite`（temp + link 提交：不覆盖 + 全文件可见），并与 `AtomicWrite` 明确分工（§4.3） |
| G21 | 没有"校验后打开"的加固 | 校验与打开之间被换成符号链接/硬链接可绕过路径策略（TOCTOU） | 已补 `OpenValidated`：unix `O_NOFOLLOW`+`O_NONBLOCK`+拒绝多链接+`SameFile`；windows 用 `SameFile`（§4.3） |
| G22 | 宿主目录解析没有覆盖环境与候选列表 | Windows/macOS 上找不到工具目录或配置目录，行为不一致 | 已补候选列表解析（`XDG_DATA_HOME` / `Library/Application Support` / `%APPDATA%`）与 `GHPIPE_CONFIG_DIR` 覆盖；解析失败明确报错（§4.5.1） |
| G23 | 参考实现的 CI 只在 Linux 跑 | 容易误以为"交叉编译通过 = 跨平台可用" | 已明确要求三平台 runner 真实执行测试，并把该差异写入 §4.5.1 与 §14 |
| G24 | 锁冲突信息不足 | 只报"已被占用"，无法判断是活跃进程还是残留锁 | 已补：锁文件写入 `pid + subject_id + purpose`，冲突时一并报出（§4.5.1） |
| G25 | 项目命令/技能脚本的 shell 语义未定义 | 在 Windows 上 `sh -c` 不存在，脚本类技能到处踩坑 | 已补：ghpipe 自身不用 shell；需要 shell 的命令必须在 `commands` 显式声明解释器与参数（§4.5.1） |
| G26 | "禁止在主分支开发 / 一 Issue 一分支"只写在流程里，GitHub 侧规则清单不全 | 本地守卫拦不住直接调 API 或手工 push 的攻击者 | 已补 §8.2 三层：GitHub ruleset（默认分支 writer 只给 Delivery App PR-only 绕过 + 特性命名空间 `ghpipe/issue-*` 只给 Developer App）、CLI 守卫、账本检测；`init checks` 安装并回读，`doctor --for handoff` 复核 |
| G27 | 合并门禁没要求"批准必须来自 Delivery App 身份" | 偷到 Developer token 的进程可以提交一个 GitHub 认可的批准 | 已补 §2.4 第 4 条：批准必须来自配置的 Delivery App 机器人并带 reviewer 身份块，其他身份的批准不计入 |
| G28 | 没有 token 威胁模型 | "防止提取其他角色 token"没有可检查的口径 | 已补 §6.6.4：按"真正的边界 → 偷到也没用 → 能发现"三层列出 9 类威胁与对策，并写明宿主隔离才是根解 |
| G29 | 远端分支删除的责任方未定义 | 容易实现成"CLI 去删远端分支"，导致依赖 agent 存活 | 已补 §8.5：远端由 GitHub `delete_branch_on_merge` 自动删，CLI 只验证与留审计声明；`done` 需要远端、本机、声明三项独立证据 |
| G30 | 安全模型建立在"不同 OS 用户/容器"上，与已定部署形态冲突 | 承诺了做不到的隔离，等于给用户虚假安全感 | 已改：§6.6.2/§6.6.2.1/§6.7 改为单机单用户前提，承诺与边界分别列表写明 |
| G31 | 高权凭据默认长期落盘 | 同用户进程可直接读取 Delivery token / Owner 密钥，暴露面被无谓放大 | 已改：Delivery token 默认 `--ephemeral` 不落盘；会话按任务短时有效；不做加密（D25，避免用复杂度掩盖不可消除的边界）（§6.7） |
| G32 | `doctor` 不报告"单机模式下哪些凭据长期在盘" | 用户看不到真实风险等级 | 已补 `doctor` 的 `isolation` 输出（mode / persistent_secrets / ephemeral / notes）（§6.7） |
| G33 | 没有代码/测试阶段的门禁路径未定义 | 文档、方案类 PR 无法合并，或被迫造必过假检查 | 已补 §10.5.1：单一检查名 + 策略随范围收紧（bootstrap 初始化验证 / strict 业务验证），文档阶段不需要业务测试 |
| G34 | 空仓库"先有规则还是先有检查"的顺序未定义 | 第一个 PR 会卡在"必需检查从未成功过"或"规则还没装" | 已补 seed → 装规则 → 全走 PR 的顺序，并用 seed SHA 上真实的成功检查作为来源（§10.5.1） |
| G35 | 文档 PR 在 strict 项目里的成本与合规未定义 | 要么每篇文档都跑全量测试，要么用路径过滤导致检查缺失 | 已补 `quality.docs_fast_path`：仍产生同名成功检查 + 必须跑真实文档检查 + 标注 `scope: docs`；禁止路径过滤（§10.5.1） |
| G36 | 只有交付切片，没有缺陷任务类型 | 缺陷只能塞进切片模板，导致"没有复现、没有回归测试、改完就说好了" | 已补 §10.7：缺陷为一等公民，含必填信息、严重度矩阵、登记权限、done 判据与模板 `templates/bug.md` |
| G37 | "有效测试"只靠 Reviewer 主观判断 | 无法证明新增测试真的能抓住该缺陷 | 已补 `ghpipe verify regression`：临时 worktree 里用 base 实现 + head 测试跑出"先红后绿"的证据（§10.7.4） |
| G38 | 偶发缺陷没有例外路径 | 无法稳定复现的缺陷会被流程永久卡住 | 已补例外：标注 `偶现` + 证据 + 针对性/防护性测试 + Reviewer 明确结论，且必须在 PR 正文声明，不允许静默跳过（§10.7.4） |
| G39 | 没有"用户提交缺陷"的入口与分诊流程 | 外部用户只能随意发 Issue，字段不全、无法直接领取，也没人能把它变成任务 | 已补 §10.11：Issue Form（`.github/ISSUE_TEMPLATE/ghpipe-bug.yml`）+ `ghpipe triage`（只读检查 / 接受 / 拒绝），接受后转成标准 bug 任务 |
| G40 | 用户提交的 Issue 与内部任务混在同一状态里 | 未分诊的报告被当作 ready 领取，或永久沉底无人处理 | 已补 `ghpipe:triage` 标签：用户提交默认不可领取，必须 Owner 分诊接受后才进入生命周期 |
| G41 | 工具自身缺陷无法回传（原设计砍掉了 error_reporting） | 项目里踩到的工具 bug 只能靠人肉转述，工具方拿不到结构化线索 | 已补 §10.12：opt-in 的设备无关上报（固定字段白名单 + 指纹去重 + 节流 + `report preview` 可预览 + 专用凭据），默认关闭 |
| G42 | 自动上报可能成为隐蔽上传或扩权通道 | 隐私风险与角色隔离被破坏 | 已补约束：默认关闭、独立凭据且只对上报仓库有 Issues 写、绝不复用业务角色凭据、只发白名单字段、可预览、失败不重试、不影响原命令退出码（§10.12） |
| G43 | 测试依赖真实监听端口 | 在 agent 沙箱内 `bind` 被拒绝，测试直接 panic；在沙箱外通过会掩盖问题 | 已定规则：测试不得绑定端口，HTTP 层用注入式假 `RoundTripper`（§14.4） |
| G44 | 测试缓存掩盖失败 | 一次沙箱外的成功会让沙箱内显示 `ok (cached)`，把真实失败藏起来 | 已定规则：验证测试必须 `-count=1`；`quality check --run-tests` 对 go test 强制该参数（§14.4） |
| G45 | 依赖 agent 间消息传递任务范围 | 实践中出现派发消息未送达、子 agent 无任务正文的情况 | 已定规则：范围/变更要求/验收标准一律落 Issue 或 PR，派发时要求先读 Issue 及评论（§14.4） |
| G46 | 没有把"宿主工具必须支持原生子 agent"写成前置条件 | 工具链不支持时可能退化成"主会话亲自开发/亲自验收"，直接破坏产品核心承诺 | 已补 §6.8：五项能力清单（独立上下文、可传任务、可收结果、独立主体、可限边界）+ 不兼容时的行为（`doctor` 报 `host_tool` 不兼容、交付类命令拒绝执行、只读命令保留、建议更换开发工具、记入 `execution log`） |
