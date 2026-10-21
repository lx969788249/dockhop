# DockHop

一条链接，把 Docker 服务连同数据从旧服务器迁到新服务器。

DockHop 是 [docker_migrate](https://github.com/lx969788249/docker_migrate) 的 Go 重写版，**还没有发布正式版本**。迁出和迁入都已完成并通过验收，正在准备发布 1.0.0；在那之前需要迁移请先用 docker_migrate。

## 怎么用

**1. 在旧服务器上迁出**

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/lx969788249/dockhop/main/run.sh)
```

选“迁出”，再选要迁移的容器（Compose 项目整组迁移）。DockHop 会短暂停止这些容器打包数据，打包完马上恢复，然后显示一条迁移链接，并一直提供下载。

**2. 在新服务器上迁入**

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/lx969788249/dockhop/main/run.sh) in '迁移链接'
```

先检查新服务器（端口、磁盘、同名服务、网段等），有问题会让你选择怎么处理；然后下载、恢复数据、按原来的配置启动服务。任何一步失败都会把新服务器还原到迁入前的样子。迁入成功后，旧服务器那边会自动结束。

旧服务器上的服务迁出后照常运行，确认新服务器没问题后再自己停掉。

## 会迁移什么

- 容器的完整配置（端口、环境变量、重启策略、健康检查、资源限制、网络和固定 IP……原样搬过去）
- 数据卷、挂载的目录和文件（保留属主、权限、软链接）
- Compose 项目：整个项目目录、目录外引用的文件（如 `env_file`），用原来的文件和项目名启动，以后照常 `docker compose up -d`
- 容器里没挂载出来的改动（只迁移改动过的文件）
- 镜像（新服务器不用联网拉取）

不迁移：宿主机本身的配置，例如 Nginx、证书、crontab、系统目录（`/etc/localtime` 这类只重建挂载关系）。

## 需要注意

- **防火墙**：新服务器要能连上旧服务器的下载端口（默认 8080）。连不上时 DockHop 会马上说明，按提示在云服务器安全组和防火墙里放行（例如 `ufw allow 8080/tcp`），迁完再关掉。两台服务器网络不通时，可以把迁移包用别的方式带过去：`dockhop in --file 迁移包 '链接'`。
- **停机时间**：取决于要打包的数据量，打包前会显示大小。容器里没挂载出来的改动特别大时（例如积攒了大量日志、崩溃转储），会在停机前提醒。
- **Compose**：新服务器需要有 `docker compose`。只有老的 docker-compose 1.x 时，有些新写法读不懂，检查时会提醒，建议先装新版。
- **系统要求**：Linux（x86_64、arm64、armv7），Docker 20.10 或更新的版本，用 root 运行。
- 迁移链接里有密钥，不要发给别人。

## 常用选项

```text
dockhop out --include web,db     只迁移这几个容器，不显示菜单
dockhop out --port 18080         换一个下载端口
dockhop in --yes '链接'          不提问，自动采用推荐的处理方式
dockhop check '链接'             只检查新服务器能不能迁入，不下载、不改动
dockhop diagnose                 出问题时生成诊断文件，可以附到 Issue 里
```

全部选项见 `dockhop --help`。

## 从 docker_migrate 过来

DockHop 是 [docker_migrate](https://github.com/lx969788249/docker_migrate) 的 Go 重写版：只依赖 Docker，不再往系统里装 jq、python3 等；链接从约 330 个字符缩短到约 100 个；迁移包整包压缩、加密，边下载边校验；失败或被中断都能还原。两者的链接和迁移包互不通用，两台服务器都用 DockHop 即可。

设计方案见 [docs/design.md](docs/design.md)。

## 开发

### 进度

| 阶段 | 内容 | 状态 |
|---|---|---|
| 0 | 补充验证 | ✅ |
| 1 | 程序骨架：命令和选项、屏幕输出、日志、Docker 客户端、加密和链接、诊断、`run.sh`、自动发布 | ✅ |
| 2 | 迁出和迁移包格式 | ✅ |
| 3 | 迁入 | ✅ |
| 4 | 发布 1.0.0 | 下一步 |

### 本地开发

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
| `internal/host` | 系统、磁盘、监听端口、文件锁 |
| `internal/spec` | 迁出、迁入共用的规则：系统路径、容器依赖、可写层改动、秘密变量；按原始配置重建容器 |
| `internal/compose` | 调用服务器上的 docker compose |
| `internal/bundle` | 迁移包格式：写入、解开、清单、迁移计划 |
| `internal/transfer` | 下载服务和下载（断点续传、进度、连不上时说明原因、迁入结果通知）、对外地址探测 |
| `internal/backup` | 迁出 |
| `internal/preflight` | 迁入前检查和用户的选择 |
| `internal/restore` | 迁入：事务日志、恢复数据和服务、回滚、中断补救 |
| `test/integration` | 在真实 Docker 上的验收测试（需要 root：`go test -c -tags integration ./test/integration`，再用 sudo 运行） |
| `test/apps` | 两台模拟服务器之间迁移真实应用（`test/apps/run.sh wordpress`，需要能访问 Docker Hub） |
| `scripts` | `run.sh` 模板、编译和测试脚本 |
