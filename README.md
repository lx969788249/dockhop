# DockHop

一条链接，把 Docker 服务连同数据从旧服务器迁到新服务器。

DockHop 是 [docker_migrate](https://github.com/lx969788249/docker_migrate) 的 Go 重写版，**目前正在开发中，还不能用来迁移**。需要迁移请先用 docker_migrate。

设计方案见 [docs/design.md](docs/design.md)。

## 开发进度

| 阶段 | 内容 | 状态 |
|---|---|---|
| 0 | 补充验证 | ✅ |
| 1 | 程序骨架：命令和选项、屏幕输出、日志、Docker 客户端、加密和链接、诊断、`run.sh`、自动发布 | ✅ |
| 2 | 迁出和迁移包格式 | 进行中 |
| 3 | 迁入 | — |
| 4 | 发布 1.0.0 | — |

## 开发

```bash
go test ./...                       # 单元测试
go test ./internal/ui -update       # 屏幕输出改了以后，更新快照
scripts/build.sh 0.0.0-dev dist     # 编译 x86_64 / arm64 / armv7，并生成带校验值的 run.sh
scripts/test-run-sh.sh dist         # 测试 run.sh（需要 Linux）
```

发布：在 main 上打 `v1.2.3` 这样的标签并推送，GitHub Actions 会自动编译、创建 Release，并更新 `run.sh` 里的版本号和校验值。

| 目录 | 内容 |
|---|---|
| `cmd/dockhop` | 程序入口 |
| `internal/cli` | 命令、选项、菜单 |
| `internal/ui` | 分步骤显示、进度、提问、结果摘要 |
| `internal/logging` | 运行日志、隐藏密钥和密码、诊断文件 |
| `internal/docker` | 精简的 Docker 接口客户端（容器配置按原始 JSON 读写） |
| `internal/crypto` | 迁移链接和加密（age） |
| `internal/host` | 系统、磁盘、监听端口 |
| `scripts` | `run.sh` 模板、编译和测试脚本 |
