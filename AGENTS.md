# ghpipe 工作约定

## 子 agent 的入口（重要）

如果你是**被派发的子 agent**（包括没有继承父会话上下文的独立上下文）：

1. 先读工作区的 **`docs/TASK.md`**（未跟踪文件，调度者写入的当前任务简报）；
2. 按简报里的角色、范围、验收条件、硬约束与汇报格式执行；
3. 如果 `docs/TASK.md` 不存在，回复 `NO_TASK` 并停止——不要自行挑选工作；
4. 不要派生下一级子 agent；不要切换或删除分支；不要 commit/push（由调度者代提交）。

## 仓库约定

- 只用 Go 标准库；`go.mod` 不新增依赖。
- 测试不得绑定监听端口（沙箱禁止 `bind`）；HTTP 层用注入式假 `RoundTripper`。
- 验证测试必须 `go test -count=1 ./...`（缓存会掩盖失败）。
- 每条 go 命令前 `export GOCACHE=/tmp/ghpipe-gocache GOPATH=/tmp/ghpipe-gopath`。
- 一个 Issue 一个分支 `ghpipe/issue-N`；`main` 只通过 squash 合并。
- 平台差异只能由 `.github/workflows/ci.yml` 的三平台矩阵判定（本地与交叉编译都不算）。

设计文档：[docs/design.md](docs/design.md)；产品方案：[docs/product.md](docs/product.md)；开发交接：[docs/handoff.md](docs/handoff.md)。
