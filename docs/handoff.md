# ghpipe 开发交接（接手 ghpipe 自身的开发）

> 用途：把 ghpipe 自身的开发工作交给任意开发工具后，用它需要的方式（原生子 agent、独立会话或人工派发）接手。本文是操作手册，不是产品文档。

## 0. 一句话状态

仓库 `ghpipe/ghpipe`，`main` 已合并到 P1b-1。打开仓库后：先跑 §1 的**宿主能力探针**，再按 §3 的流程接手任务（下一个任务是 Issue #7）。

| 项 | 值 |
|---|---|
| 仓库 | `github.com/ghpipe/ghpipe`（public） |
| 当前 main | 见 `git log --oneline -1` |
| CI | `.github/workflows/ci.yml`：三平台测试矩阵（ubuntu/macos/windows）+ 六目标交叉编译；分支推送即触发 |
| 已完成 | Issue #1（`internal/github` 传输/分页/GraphQL/错误分类）、#3（`internal/gitx` + `internal/lifecycle`）、#4（cwd 跨平台校验 bug）、#2（Link 逗号 bug） |
| 待办 | **#7（P1b-2：只读面命令）**、#5（P1b-1 follow-up） |
| 规则出处 | [design.md §6.8](design.md)（宿主能力判定）、§14.4（工程硬规则）、[product.md](product.md)（对开发工具的要求） |

### 0.1 如果你是子 agent：先读任务简报

**当前任务简报固定放在工作区的 `docs/TASK.md`（未跟踪文件，由调度者写入）。** 收到派发后第一件事就是打开它，按里面的范围、验收条件、硬约束与汇报格式执行；它优先于你从其它文件推断出的任何结论。

如果 `docs/TASK.md` 不存在，说明当前没有派发给你的任务：请回复 `NO_TASK` 并停止，不要自行挑选工作。

（写成文件的理由：不同工具的派发通道不同，任务正文落仓库/账本才是可靠来源；详见 design §6.8.5。）

## 1. 先跑宿主能力探针（按结果选通道，不要凭印象下结论）

**任务唯一载体是 Issue/PR**：派发只传任务标识（任务名/分支名里的 `issue-<N>`），子 agent 自己读 Issue；不允许用本地文件承载任务正文。

**2026-09-13 实测**：工具能派发独立上下文子 agent（上下文独立、结果可收集），但本环境的子 agent 沙箱**无网络**，读不到 Issue；派发消息字段也不会进入子 agent 上下文。因此在本环境当前配置下，"独立子 agent + 任务只来自 Issue"这条链路不成立，按 `no_task_delivery` 处理，走降级形态（独立会话/进程或人工派发）并在账本记录 `independence`。

复跑探针的做法：生成三个随机 nonce：`N_A`、`N_B`、`N_X`；**先派 B**（只给 `N_B`）并等它回报，**再派 A**（只给 `N_A`，并问它"你还能看到哪些 nonce"），`N_X` 谁都不给。判据：

1. A 回报 `N_A`、B 回报 `N_B` → **任务投递**成立；
2. A 报不出 `N_B`/`N_X`、B 报不出 `N_A`/`N_X` → **上下文独立**成立；
3. 主会话在超时内收到两份结果 → **结果可收集**成立。

三项全过才判 `compatible`（级别 L1/L2/L3）。失败按类别处置（design §6.8.3）：

| 分类 | 现象 | 处置 |
|---|---|---|
| `no_dispatch` | 工具没有派发能力 | 判 L0，走降级阶梯（§6.8.4） |
| `no_task_delivery` | 子 agent 起来了但没拿到任务（本项目实测遇到的就是这类） | **换派发通道**：把任务写到 `docs/TASK.md` 并在派发指令里要求先读它；或改用工具自己的 agent 定义（自定义 subagent 文件 / `@` 点名）；或打开「继承父上下文」的派发选项 |
| `shared_context` | 子 agent 能看到父会话或彼此内容 | **不得用于验收**；换 L2/L3 或用工具的隔离选项 |
| `no_collect` | 只能后台跑、拿不到结果 | 结果走文件/账本回收，状态标注 `collection: via_ledger` |
| `restricted_absent` | 无法限制验收者写权限 | CLI 侧补偿（Reviewer 会话本身没有 `commit`/`push` 能力）并在账本标注 |

探针结果写 `.ghpipe/state/host-probe.json` 并在当前 Issue 留一条摘要评论；`doctor --for handoff` 据此判定 `unprobed` / `compatible` / `incompatible`。

## 2. 工程约束（踩过的坑，必须遵守）

| 约束 | 原因 |
|---|---|
| 测试**不得绑定监听端口** | agent 沙箱禁止 `bind`，`httptest.NewServer` 会直接 panic。HTTP 层测试用注入式假 `RoundTripper`；需要真实 HTTP 行为时用 `httptest.NewRecorder` |
| 验证测试必须 `go test -count=1` | 测试缓存会让一次沙箱外的成功显示成 `ok (cached)`，掩盖真实失败 |
| 平台差异只能由 CI 判定 | 本地沙箱与交叉编译都发现不了平台语义差异（例：#4 的 `filepath.IsAbs("/tmp")` 只在 Windows 暴露） |
| 一个 Issue 一个分支 `ghpipe/issue-N` | 禁止在 `main` 上提交；`main` 只通过 squash 合并演进 |
| 任务范围/变更要求/验收标准必须落 Issue 或 PR | agent 间消息可能投递失败；账本是权威来源，派发时要求子 agent 先读任务简报与 Issue |
| 每个工作树用独立 `GOCACHE`（如 `GOCACHE=/tmp/ghpipe-gocache`） | 沙箱可能禁止写 `~/Library/Caches` |
| 只依赖标准库 | `go.mod` 目前只有 `module` 与 `go` 两行；新增依赖需先讨论 |

## 3. 每个任务的流程（不可省略独立验收）

1. 主会话读 Issue，确认范围、验收条件、测试要求与「不在范围」，并把任务简报写到 `docs/TASK.md`。
2. 用**原生子 agent** 派发 Developer：独立上下文、独立主体，工作在 `ghpipe/issue-N` 分支，实现 + 测试 + 自检（`go vet`、`go test -count=1 ./...`、六目标交叉编译）。
3. Developer 交付固定 SHA；主会话核对改动只属于该范围。
4. 推分支触发 CI（三平台 + 六目标），**必须全绿**才能进入验收。
5. 用**另一个原生子 agent** 派发 Reviewer（不复用 Developer 上下文）：在固定 SHA 上独立核验，检查每条 AC、尝试证伪（变异测试/对抗性输入）、确认没有改动文件，给出 `APPROVE` 或 `REQUEST_CHANGES` + 证据。
6. `REQUEST_CHANGES` → 回原 Developer 修复（新 SHA 需重新验收）；`APPROVE` → 主会话 squash 合并到 `main`、关闭 Issue、删除分支（本地 + 远端）。

## 4. 按需参考

- 产品与流程：[product.md](product.md)
- 技术设计：[design.md](design.md)（模块划分、配置、命令面、授权、跨平台、自动化、决策清单 §17、缺口清单 §18）
- 第一次自举的复盘与行动清单：[retro-selfhosting.md](retro-selfhosting.md)
- 当前任务规格：Issue #7（P1b-2 只读面）、Issue #5（P1b-1 follow-up）

## 5. 接手后的第一批动作

1. 跑 §1 的探针；把结论（工具、版本、级别、失败分类）作为评论记到 Issue #7。
2. 读 Issue #7，把任务简报写进 `docs/TASK.md`，按 §3 派发 Developer。
3. 若发现设计缺口，先改 `docs/design.md`（或新开 Issue），不要边写代码边改契约。
