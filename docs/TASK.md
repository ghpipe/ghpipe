# TASK: Issue #7 [P1b-2] 只读面命令 status / doctor --offline / metadata（只读）+ checks 事实

你扮演 **Developer**。独立实现本任务；完成后如实汇报，不要声称未验证的事情。

## 0. 环境与边界

- 仓库：`/Users/ws/code/ghpipe`（cwd 已经是它），模块 `github.com/ghpipe/ghpipe`，仅标准库。
- 分支：`ghpipe/issue-7`（已检出，基线 = 当时的 `main`）。**禁止**在 `main` 上提交。
- 参考（只读）：`docs/design.md`、`docs/product.md`、`docs/handoff.md`，以及已合并的 `internal/github`、`internal/gitx`、`internal/lifecycle`、`internal/project`、`internal/cli`、`internal/result`。
- 先读 `docs/handoff.md` §2「工程约束」并遵守。
- 提交：如果你有 git 写权限就提交到 `ghpipe/issue-7`（提交信息 `feat: ... (#7)`），**不要** push、不要动 `main`、不要动远端。如果没有 git 写权限，就只在工作区留下改动，并在汇报里写明「未提交」。

## 1. 目标

实现**只读**的交接状态面（P1b 第二段）。本切片不产生任何远端写入。

## 2. 范围

### 2.1 `internal/metadata`

- `Facts(issue N)`（只读）：
  - Issue：状态、labels、milestone、body。
  - 关联 PR：GraphQL `timelineItems(itemTypes:[CROSS_REFERENCED_EVENT])` 找**同仓库**、base = 默认分支的 PR；再用「正文中唯一独占一行的 `Closes #N`」确认归属（复用/对齐 `internal/attribution` 的解析；不存在就在本包内实现等价解析并加测试）。出现第二个关闭引用、多个活动 PR、跨仓库引用 → 明确报错，不猜。
  - 打开中的 PR 的 reviews（分页必须完整）。
  - 分支 `ghpipe/issue-N` 是否存在（`internal/gitx` 或 REST ref 读取）。
- `Projection(facts)`：调用 `internal/lifecycle.Plan`，产出「每个对象 → 阶段」+ 期望标签集合（`ghpipe:ready|active|review|changes-requested|closing|done|cancelled`）；PR 继承 Issue 的非阶段标签，PR 的 milestone 必须为空。
- `Differences(issue, prs, desired)`：只计算差异，不写入。

### 2.2 `internal/checks`

- 从配置读 `ci.required_checks`（context + integration_id，唯一）。
- `GET /repos/{owner}/{repo}/rules/branches/{branch}` 读有效规则，校验必需检查的**来源绑定**（context 相同但 integration_id 不同 → failed；规则不可读 → unknown）。
- commit `statusCheckRollup`（GraphQL 分页）：返回的 `oid` 必须等于请求 SHA、`CheckRun.checkSuite.commit.oid` 必须等于请求 SHA；同名 check-run 与 commit status 同时存在时必须**都**成功；`skipped`/`neutral` 保留原始 conclusion 但不称为「测试通过」。

### 2.3 `internal/status`

按 design §10.1 输出结构化状态：`target(kind/number)`、`checkout(branch/head)`、`role`、`issue`、`pr`、`stage`、`reviews`（含原生 `reviewDecision`）、`checks`（每个必需检查的状态与来源）、`blockers[{code,severity}]`、`next_actions`、`observed_at`。

- 严重度合并：`failed > unknown > pending > ready`；退出码 0 = ready，1 = 其他。
- 已合并 PR 的 checks 绑定 `merge_commit_sha`，并另行列当前默认分支 head。
- 结束前回读 PR head；两次不一致 → `head_changed`（failed）。

### 2.4 命令（接入 `internal/cli` 命令树）

- `ghpipe status (--issue N | --pr N) --role developer|delivery [--offline] [--json]`
  - `--offline`：**不读凭据、不联网**；远端事实一律 unknown，只用本地 config + `gitx` 事实。
- `ghpipe doctor --offline --for plan|develop|review`
  - 只读本地：CLI/资源版本、配置校验；`--for develop|review` 额外做分支守卫相关的本地检查。不读凭据、不联网、不运行项目命令。
- `ghpipe metadata (--issue N | --pr N) [--json]`
  - **只读**：输出期望投影与差异。**不得**提供 `--apply`。
- 所有命令走 `internal/result` 的统一信封与退出码（0 成功 / 1 确定失败 / 2 结果未知 / 3 用法或前置错误）。

## 3. 验收条件（必须逐条自证）

- **AC1** 关联 PR 判定只用「同仓库 + base=默认分支 + 唯一独占一行 `Closes #N`」；第二个关闭引用、多个活动 PR、跨仓库引用各有测试（用注入式假 transport 构造 GraphQL/REST 响应）。
- **AC2** `Differences` 只计算不写入；期望标签集合与 PR 继承规则（继承 Issue 非阶段标签、PR milestone 置空）有测试。
- **AC3** 来源不匹配 → failed；规则不可读 → unknown；rollup `oid` 与请求 SHA 不一致 → 契约错误；同名 check-run 与 commit status 并存必须都成功（各有测试）。
- **AC4** 离线模式**不读取任何凭据文件**（测试断言未触碰凭据路径）、远端事实 unknown、退出码 1；在线模式断言一律用假 transport，不访问真实 GitHub。
- **AC5** 严重度合并与 `next_actions`：每个 blocker code 至少一个用例；`done` 时 `next_actions` 为空。
- **AC6** `metadata` 命令无 `--apply`；调用后无任何非 GET 请求（假 transport 断言只收到 GET）。
- **AC7** 只用标准库；`go.mod` 不变；**测试不得绑定监听端口**（禁止 `httptest.NewServer`，用注入式 `RoundTripper` / `httptest.NewRecorder`）；`go test -count=1` 在沙箱内全绿；六目标交叉编译通过。

## 4. 不在范围

任何写操作（`--apply`、标签/里程碑写入、comment）、凭据与 token、`--connectivity`、`init`/`publish`/`release`/`task` 命令、`quality check`（下一片 P1b-3）；不要改 `internal/github` 与 `internal/lifecycle`（如确需改动，先停下并在汇报里提出，别自行改契约）。

## 5. 自检（必须真跑，附原始输出）

```
export GOCACHE=/tmp/ghpipe-gocache
go vet ./...
go test -count=1 ./...
for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 go build ./... || exit 1
done
```

## 6. 汇报格式（最后一条消息）

1. `STATUS: DONE | BLOCKED`
2. 提交 SHA（或「未提交」）与分支名
3. 变更文件清单（新增/修改，一句话说明每个文件的作用）
4. 逐条 AC 的自证：每条给出证据（测试名 + 关键断言/命令输出摘要）
5. 上面 5 条自检命令的**原始输出摘要**（通过/失败，失败贴报错）
6. 已知不足、未覆盖的边界、需要 Reviewer 重点验证的地方
