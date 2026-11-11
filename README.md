<div align="center">

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/logo-dark.png">
  <img alt="DockHop" src="docs/images/logo-light.png" width="420">
</picture>

**只需两步，完整迁移你的 Docker**

[![CI](https://github.com/lx969788249/dockhop/actions/workflows/ci.yml/badge.svg)](https://github.com/lx969788249/dockhop/actions/workflows/ci.yml)
[![Apps](https://github.com/lx969788249/dockhop/actions/workflows/apps.yml/badge.svg)](https://github.com/lx969788249/dockhop/actions/workflows/apps.yml)
[![Release](https://img.shields.io/github/v/release/lx969788249/dockhop)](https://github.com/lx969788249/dockhop/releases/latest)
[![Stars](https://img.shields.io/github/stars/lx969788249/dockhop?style=flat&logo=github)](https://github.com/lx969788249/dockhop/stargazers)
![Platform](https://img.shields.io/badge/platform-Linux-FCC624?logo=linux&logoColor=black)

[快速开始](#快速开始) · [迁移哪些东西](#迁移哪些东西) · [更多用法](#更多用法) · [常见问题](#常见问题)

</div>

DockHop 是一个 Docker 迁移工具：一个脚本即可完整迁移你的docker服务，迁完就能直接用。主打安全、可靠、易用。

## 特点

- **简单**：两台服务器运行同一条命令，一台选「迁出」，一台选「迁入」，按步骤即可完成迁移。不用记配置、不用记参数、这些都由脚本来搞定。
- **完整迁移**：镜像、数据卷、挂载目录、端口、环境变量、网络、Compose 项目都会迁移，容器里改过的文件也不丢。完整迁移
- **可靠**：严格的测试流程，保证使用的可靠性。你能想到的问题点，我都有考虑。
- **安全**：数据加密传输，不怕中间人劫持或篡改。保证迁移安全。给你满满的安全感。
- **干净**：只需要 Docker，不需要安装其他依赖；运行过程中的中间产物，包括脚本自身也是用完即删。不会在服务器上面乱拉屎

## 快速开始

在旧服务器和新服务器上运行同一条命令：

```bash
bash <(curl -fsSL https://dockhop.run)
```

**第一步，旧服务器**：选 `[1] 迁出`，选好要迁的服务。打包完会显示一条迁移链接，复制它。这个窗口先别关，新服务器要从这里下载数据。

**第二步，新服务器**：选 `[2] 迁入`，粘贴刚才复制的链接。检查、下载、恢复都会自动完成，迁入成功后旧服务器那边也会自动结束。

> [!NOTE]
> 迁移链接里含有解密密钥，不要发给别人。新服务器上也只粘贴你自己旧服务器上显示的链接：迁入会照着迁移包在这台服务器上建容器、写文件。

> [!TIP]
> 新服务器连不上，几乎都是旧服务器没有放行端口（默认 TCP 8080）。在云服务商的安全组和服务器防火墙里放行就行。

## 迁移哪些东西

- 镜像：和旧服务器上的版本一模一样。新服务器上已经有、或者能从镜像仓库拉到同一个镜像时，就不从旧服务器传
- 数据卷、挂载的目录和文件，属主和权限保持不变
- 容器里没挂载出来、但改过的文件
- 端口、环境变量、启动命令、重启策略、资源限制、健康检查等全部运行参数
- 自定义网络、固定 IP、网络别名
- Docker Compose 项目：整个项目目录和它引用的文件，迁移后照常 `docker compose up -d`
- 容器原来的状态：运行、暂停或停止

## 系统要求

- Linux，x86_64、arm64 或 armv7，用 root 运行
- Docker 20.10 或更新的版本
- 迁移 Compose 项目时，新服务器要装好 `docker compose`
- 一行命令需要 `curl`（或 `wget`）和 `sha256sum`，常见的发行版都自带
- 新服务器要能连上旧服务器的一个 TCP 端口（默认 8080）

## 更多用法

一般按菜单走就够了。需要的时候，把选项加在命令后面，例如：

```bash
bash <(curl -fsSL https://dockhop.run) out --port 9000
```

| 选项 | 作用 |
|---|---|
| `out --port 端口` | 下载服务用的端口，默认 8080。安全组只放行了某个端口时用它 |
| `out --host 地址` | 链接里用的 IP 或域名。自动探测不准（NAT、多网卡），或者想走内网时用 |
| `out --include a,b` | 只迁移这几个容器（或它们所在的 Compose 项目），不显示菜单 |
| `out --stop-shared` | 允许临时停掉和所选容器共用数据的其他容器，打包完自动恢复 |
| `in --yes` | 检查发现的问题自动按推荐的方式处理 |
| `in --existing replace\|skip\|fail` | 新服务器已有同名服务时：替换、保留已有的，或者停止迁入 |
| `in --keep-succeeded` | 有服务起不来时，保留已经正常运行的，只删除起不来的 |

| 环境变量 | 作用 |
|---|---|
| `DOCKHOP_HOME` | DockHop 放文件的目录，默认 `~/.dockhop`。系统盘不够大时，换到大一点的盘 |
| `DOCKHOP_MIRROR` | 下载程序用的镜像站。不设置时，国内先用 Gitee，其他地方先用 GitHub |
| `DOCKHOP_NO_STATS=1` | 不统计、也不显示迁移次数 |

完整说明见 `dockhop --help`。

## 常见问题

<details>
<summary><b>新服务器连不上怎么办？</b></summary>

多半是端口没放行。在旧服务器的云服务商安全组和防火墙里放行 TCP 8080（或者 `--port` 指定的端口），例如 `ufw allow 8080/tcp`，然后在新服务器上用同一个链接重新迁入。

链接里的 IP 不对（NAT、多网卡）时，在旧服务器上用 `out --host 公网IP` 重新迁出。
</details>

<details>
<summary><b>旧服务器要停机多久？</b></summary>

只在打包数据时停一下，打包完马上恢复。小服务不到 1 秒，几百 MB 的数据库几十秒。导出镜像和传输都不影响旧服务器上的服务。
</details>

<details>
<summary><b>迁入失败了，新服务器会不会被弄乱？</b></summary>

不会。迁入失败或按了 Ctrl+C，这次迁入的内容会被删掉，被替换的原有服务放回原处。删除、放回的过程中再按 Ctrl+C，会先提醒现在停止可能留下没收拾完的东西（下次运行会接着处理），确定要停止就再按一次。处理好问题后用同一个链接重新迁入，已经下载好的不用重新下载。

程序被强制结束（比如断电）也没关系，下次运行时会提示先还原。
</details>

<details>
<summary><b>新服务器上已经有同名的容器或 Compose 项目？</b></summary>

迁入前会问你：替换它（先备份，迁入失败时放回原来的），还是保留已有的、跳过这一项。

数据卷和目录也一样：新服务器上已有同名的数据卷，或者同一个目录里已经有东西时，迁入前会提示，迁移过来的数据会替换它们（同样先备份，失败时放回原来的）。
</details>

<details>
<summary><b>一次迁了好几个服务，只有一个起不来？</b></summary>

会问你全部删除（推荐），还是只删起不来的、保留已经正常运行的。之后用同一个链接重新迁入，已经迁好的会跳过。
</details>

<details>
<summary><b>为什么有的容器镜像名变成了 <code>dockhop-image:…</code>？</b></summary>

两种情况：这个容器里删过镜像自带的文件；或者它用的镜像在旧服务器上已经被更新掉，实际跑的是旧版本。DockHop 迁的是容器实际在用的内容，所以给镜像起了这个名字。以后想升级，用原来的镜像名重新创建就行（容器里没挂载出来的改动会丢）。
</details>

<details>
<summary><b>需要多少磁盘空间？</b></summary>

两边都会提前算好：旧服务器在停机之前检查，新服务器在下载之前检查，不够就先停下。系统盘不够时，在命令前面加上 `DOCKHOP_HOME=/大一点的盘/dockhop`。
</details>

<details>
<summary><b>会在服务器上留下文件吗？</b></summary>

不会。程序每次运行完就删掉，迁移成功后 `~/.dockhop` 也会删掉。迁入失败时会留下日志和下载好的迁移包，方便重试，不迁了可以直接删掉。
</details>

<details>
<summary><b>迁移次数是怎么统计的？会上传我的信息吗？</b></summary>

迁入成功后，程序向 `dockhop.run` 发一个空请求，成功次数加 1；迁出、迁入动手之后失败时也发一个，失败次数加 1（用来改进稳定性；还没改动服务器就停下的不算）。统计里只存次数，不存 IP，也不存任何服务器信息（IP 只用来防止有人刷次数），次数和成功率在 https://dockhop.run/stats 能看到。不想统计的话，运行前设置 `DOCKHOP_NO_STATS=1`。

另外，旧服务器的网卡上没有公网地址时（例如在 NAT 后面），迁出会访问 api.ipify.org 等网站查出自己的公网 IP，写进迁移链接；用 `out --host 公网IP` 指定了就不查。
</details>

<details>
<summary><b>链接被别人拿到了会怎样？</b></summary>

旧服务器的下载服务开着的时候，拿到完整链接的人能下载并解开你的数据；只拿到 `#` 前面那部分的人什么也下载不了。迁入成功后链接自动失效；不想迁了，在旧服务器上按 Esc 再按回车，链接马上失效。
</details>

<details>
<summary><b>出错了怎么反馈？</b></summary>

失败时结果里会写出日志的位置，提交 [Issue](https://github.com/lx969788249/dockhop/issues) 时附上它。日志里的密钥和密码已经自动隐藏。
</details>

## 参与开发

DockHop 用 Go 编写，编译成一个不依赖系统库的程序。设计说明见 [docs/design.md](docs/design.md)，每个版本在设计上改了什么、为什么改见 [docs/changelog.md](docs/changelog.md)。

```bash
go test ./...                                # 单元测试
go test ./internal/ui -update                # 屏幕输出改了以后，更新快照
scripts/build.sh 0.0.0-dev dist              # 编译三个架构，生成带校验值的 run.sh
go test -c -tags integration -o dockhop.test ./test/integration
sudo DOCKHOP_BIN=… ./dockhop.test -test.v    # 在真实 Docker 上的验收测试（需要 root）
test/apps/run.sh wordpress                   # 在两台模拟服务器之间迁移一个真实应用
```

一行命令用的 `run.sh` 是发布时由 `scripts/run.sh.in` 生成的，不要直接改它。

改了设计时，design.md 直接改成新的样子，改了什么、为什么改记在 changelog.md。修缺陷和加功能分开提交，方便单独回滚和审查。

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=lx969788249/dockhop&type=Date)](https://star-history.com/#lx969788249/dockhop&Date)

## 许可证

[GPL-3.0](LICENSE)：可以自由使用、修改和分发；分发修改后的版本时，也要以 GPL-3.0 公开源代码。
