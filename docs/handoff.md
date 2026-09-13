# ghpipe 开发交接（换开发工具后从这里开始）

> 用途：把 ghpipe 自身的开发工作交给一个**支持原生子 agent** 的开发工具。本文是操作手册，不是产品文档。

## 0. 一句话状态

仓库 `ghpipe/ghpipe`，`main` 已合并到 P1b-1；打开仓库后先跑下面的**兼容性探针**，通过后再按 §3 的流程接手任务。

| 项 | 值 |
|---|---|
| 仓库 | `github.com/ghpipe/ghpipe`（public） |
| 当前 main | 见 `git log --oneline -1`（本文写作时为 `add3fb3`） |
| CI | `.github/workflows/ci.yml`：三平台测试矩阵（ubuntu/macos/windows）+ 六目标交叉编译；分支推送即触发 |
| 已完成 | Issue #1（`internal/github` 传输/分页/GraphQL/错误分类）、#3（`internal/gitx` + `internal/lifecycle`）、#4（cwd 跨平台校验 bug）、#2（Link 逗号 bug） |
| 待办 | **#7（P1b-2：只读面命令）**、#5（P1b-1 follow-up：评审输入契约、浅克隆与 ref 守卫用例、§8.1 文档） |
| 阻塞机制 | 见 [design.md §6.8](design.md)：宿主工具必须支持原生子 agent，否则报不兼容并换工具（不再降级为主会话自审自过） |

## 1. 先跑兼容性探针（不通过就不要开工）

让主会话派发**两个原生子 agent**，A 只拿到令牌 `TOKEN_A`、B 只拿到 `TOKEN_B`，要求各自回报 `TASK_OK: <自己拿到的令牌>`。三项全过才算兼容：

1. 两个子 agent 都回报了正确令牌（**任务可投递**）；
2. A 报不出 `TOKEN_B`、B 报不出 `TOKEN_A`（**上下文独立**）；
3. 主会话能收到两份结果（**可等待与收集**）。

任一项失败 → 判定不兼容，换工具；**不要**让主会话亲自开发或亲自验收。

## 2. 工程约束（踩过的坑，必须遵守）

| 约束 | 原因 |
|---|---|
| 测试**不得绑定监听端口** | agent 沙箱禁止 `bind`，`httptest.NewServer` 会直接 panic。HTTP 层测试用注入式假 `RoundTripper`；需要真实 HTTP 行为时用 `httptest.NewRecorder` |
| 验证测试必须 `go test -count=1` | 测试缓存会让一次沙箱外的成功显示成 `ok (cached)`，掩盖真实失败 |
| 平台差异只能由 CI 判定 | 本地沙箱与交叉编译都发现不了平台语义差异（例：#4 的 `filepath.IsAbs("/tmp")` 只在 Windows 暴露） |
| 一个 Issue 一个分支 `ghpipe/issue-N` | 禁止在 `main` 上提交；`main` 只通过 squash 合并演进 |
| 任务范围/变更要求/验收标准必须落 Issue 或 PR | agent 间消息可能投递失败；账本是权威来源，派发时要求子 agent 先读 Issue 与评论 |
| 每个工作树用独立 `GOCACHE`（如 `GOCACHE=/tmp/ghpipe-gocache`） | 沙箱可能禁止写 `~/Library/Caches` |
| 只依赖标准库 | `go.mod` 目前只有 `module` 与 `go` 两行；新增依赖需先讨论 |

## 3. 每个任务的流程（不可省略独立验收）

1. 主会话读 Issue，确认范围、验收条件、测试要求与「不在范围」。
2. 用**原生子 agent** 派发 Developer：独立上下文、独立主体，工作在该 Issue 的分支上，实现 + 测试 + 自检（`go vet`、`go test -count=1 ./...`、六目标交叉编译）。
3. Developer 交付固定 SHA；主会话核对改动只属于该范围。
4. 推分支触发 CI（三平台 + 六目标），**必须全绿**才能进入验收。
5. 用**另一个原生子 agent** 派发 Reviewer（不复用 Developer 上下文）：在固定 SHA 上独立核验，检查每条 AC、尝试证伪（变异测试/对抗性输入）、确认没有改动文件，给出 `APPROVE` 或 `REQUEST_CHANGES` + 证据。
6. `REQUEST_CHANGES` → 回原 Developer 修复（新 SHA 需重新验收）；`APPROVE` → 主会话 squash 合并到 `main`、关闭 Issue、删除分支（本地 + 远端）。

## 4. 按需参考

- 产品与流程：[product.md](product.md)（能干什么、业务流程、缺陷入口、对开发工具的要求）
- 技术设计：[design.md](design.md)（模块划分、配置、命令面、授权、跨平台、自动化、决策清单 §17、缺口清单 §18）
- 当前任务规格：Issue #7（P1b-2 只读面）、Issue #5（P1b-1 follow-up）

## 5. 接手后的第一批动作

1. 跑 §1 的兼容性探针；通过后在本 Issue 留一条评论记录结论（时间、工具、版本）。
2. 读 Issue #7，按 §3 流程派发 Developer。
3. 若发现设计缺口，先改 `docs/design.md`（或新开 Issue），不要边写代码边改契约。
