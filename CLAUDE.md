# DockHop 开发守则

改代码之前先读这份守则，再读 docs/design.md 里和这次改动有关的章节。能用程序检查的规矩都写成了测试（internal/codecheck，`go test ./...` 会跑），这里写的是程序查不了、要靠判断的，以及每条规矩为什么这样定。规矩跟着代码一起改、一起提交。

## 结构

1. **同一件事只在一处定义。** 要写到新服务器的哪些地方、要打包哪些地方、谁在用这些数据、每种事务日志记录要哪些字段，都只算一份（`preflight.WritesOf`、`hostRoots`、`spec.WritesData` 和 `UsesData`、`opRules`）。以前这些各算几遍、规则还对不上，v1.2.8 修的缺陷大多出在这里。要用就调用那一份，不要照着再写一遍。
2. **一种数据一种形态。** 一种记录只表示一种情况，不靠“某个字段空不空、某个路径在不在里面”区分两种意思。以前事务日志这样做，改一处漏一处。
3. **结构里的字段从建起来就有效。** 走到某一步才有的内容，单独成一个结构往下传（迁入的 checked → bundleFiles → scope → migrate），不要放进一个大结构、靠调用顺序保证它有值。
4. **逻辑和界面分开。** 迁入、迁出、预检的逻辑出错时返回 `*ui.Failure`（原因、细节、建议，和不显示的类别 Kind），不自己显示；显示只在 Run 一处。要分情况处理的给 Failure 一个 Kind，测试按 Kind 判断，不比对屏幕上的中文。codecheck 查 StepFailed 用在哪里。
5. **测试的东西不进正式代码。** 测试要改短的时限、要模拟的失败只经 `testpoint.Settings`（Deps.Test）进来，只有测试版（`-tags testpoint`）从环境变量读；正式代码只在 cli 读环境变量；测试不改包级变量。codecheck 都查。
6. **包分层。** 每个包在第几层写在 codecheck 的 `layers` 里，只能用更低层的包；restore 和 backup 互不依赖，cli 把它们接起来。要加新的依赖先想清楚为什么，改表时写明。
7. **函数只做一件事。** codecheck 量认知复杂度（超过 45）和嵌套（超过 4 层）：线按本项目自己的数据定，抓的是最难读的那 1%。行数不设线：长短和难读只是中等相关，平铺的长函数（逐个结局写结果摘要、一条条配置规则）不用拆。超了线先找自然的拆分点，一件事一个函数；拆开反而要来回传一堆变量、跳着看的，不拆，在 `exceptions` 里写明为什么。例外的数字只许变小，“待整理”的有空就拆掉。

## 做法

1. 修根本原因，不打补丁，不留兼容旧逻辑的代码。
2. 修缺陷先写会失败的测试，再改代码；整理结构时行为不变。修缺陷和整理结构分开提交。
3. 测试用的假 Docker 只用 `internal/docker/dockertest`；测试里的 run、migrate、undo 用 helpers_test.go 里的 testRun、testMigrate、testUndo 造（codecheck 查）。
4. 改完要过：gofmt（用 go.mod 定的 Go 版本的：在仓库里运行 `$(go env GOROOT)/bin/gofmt`，不同版本的对齐方式偶尔不一样）、`go vet ./...` 和 `go vet -tags testpoint ./...`、`go test ./...`。动了迁移流程的，用测试版在真实 Docker 上跑验收测试（命令见 README 的“参与开发”）。
5. 改了设计，docs/design.md 直接改成新的样子；改了什么、为什么，记在 docs/changelog.md 的“下一个版本”里。
6. 发布前做一次结构体检：`go test ./internal/codecheck -run Report -v`，看复杂度最高的几个和例外表有没有变多，记进改动记录；“待整理”的越积越多，就先整理再加功能。
7. 仓库是公开的：提交里不能有服务器地址、密钥、迁移链接。
