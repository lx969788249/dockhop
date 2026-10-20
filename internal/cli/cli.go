// Package cli 解析命令和选项，把工作分派给迁出、迁入、检查和诊断。
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/lx969788249/dockhop/internal/backup"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 退出码。
const (
	ExitOK          = 0
	ExitFailed      = 1
	ExitUsage       = 2
	ExitInterrupted = 130
)

// Env 是程序运行的环境，测试时可以替换。
type Env struct {
	Stdout   io.Writer
	Stderr   io.Writer
	TTY      bool      // 标准错误是不是终端（决定颜色和进度的显示方式）
	Input    io.Reader // 可以提问时是终端输入，否则为 nil
	Getenv   func(string) string
	Version  string
	Workdir  string // 当前目录，诊断文件写在这里
	Context  context.Context
	HomeDir  func() (string, error)
	Platform string // 例如 linux/amd64
	Args     []string
	verbose  bool
}

func (e *Env) fill() {
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.Context == nil {
		e.Context = context.Background()
	}
	if e.HomeDir == nil {
		e.HomeDir = os.UserHomeDir
	}
	if e.Platform == "" {
		e.Platform = runtime.GOOS + "/" + runtime.GOARCH
	}
	if e.Version == "" {
		e.Version = "dev"
	}
}

// dockhopHome 是日志、记录和工作文件的目录：DOCKHOP_HOME，默认 ~/.dockhop。
func (e *Env) dockhopHome() string {
	if h := e.Getenv("DOCKHOP_HOME"); h != "" {
		return h
	}
	home, err := e.HomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".dockhop")
}

const helpText = `DockHop：一条链接，把 Docker 服务连同数据从旧服务器迁到新服务器。

用法：
  dockhop                     显示菜单
  dockhop out [选项]          迁出：打包这台服务器上的服务，生成迁移链接（在旧服务器上运行）
  dockhop in [链接] [选项]    迁入：将服务迁到这台服务器（在新服务器上运行）
  dockhop check 链接          只检查这台服务器能不能迁入，不下载、不改动
  dockhop diagnose            收集诊断信息，生成一个可以附到 Issue 里的文件
  dockhop version             显示版本

迁出选项：
  --include a,b        只迁移指定的容器（或它们所在的 Compose 项目），不显示菜单
  --no-stop            不停机打包（数据库等的数据可能不一致）
  --stop-shared        不能提问时，允许临时停止和所选容器共用数据的其他容器
  --snapshot 方式      容器里没有挂载出来的改动：auto 有改动才迁移（默认）、always 总是、never 不迁移
  --port 端口          下载服务的端口（默认 8080，被占用时自动往后找）
  --host 地址          链接里使用的公网 IP 或域名（默认自动探测）
  --workdir 目录       存放迁移包的目录（默认 ~/.dockhop/work）

迁入选项：
  --file 迁移包        用别的方式（scp、网盘等）带过来的迁移包，不再下载
  --yes                不能提问时，预检发现的问题自动采用推荐做法
  --existing 方式      已有同名服务时：replace 替换（可回滚，默认）、skip 跳过、fail 停止
  --skip-check         跳过迁移前预检
  --health-timeout 秒  健康检查至少等待的秒数（默认 60，会按容器自己的检查间隔自动加长）
  --keep-files         迁入成功后保留下载和解压的文件

通用选项：
  -v, --verbose        在屏幕上显示每一步的细节
  -h, --help           显示帮助

环境变量：
  DOCKHOP_HOME         日志、记录和工作文件的目录（默认 ~/.dockhop）
  DOCKHOP_KEEP_LOG=1   成功后也保留日志

迁移链接里有特殊字符，在命令里使用时请用单引号括起来：dockhop in '链接'
`

// Run 运行一条命令，返回退出码。
func Run(args []string, env Env) int {
	env.fill()
	verbose := false
	// 通用选项可以写在子命令前后任意位置。
	var rest []string
	for _, a := range args {
		switch a {
		case "-v", "--verbose":
			verbose = true
		case "-h", "--help", "help":
			fmt.Fprint(env.Stdout, helpText)
			return ExitOK
		case "--version":
			rest = append(rest, "version")
		default:
			rest = append(rest, a)
		}
	}
	cmd := ""
	if len(rest) > 0 {
		cmd, rest = rest[0], rest[1:]
	}
	env.verbose = verbose
	env.Args = args
	screen := ui.New(env.Stderr, env.TTY, nil)
	screen.SetVerbose(verbose)
	err := dispatch(cmd, rest, &env, screen)
	return exitCode(err, env.Stderr)
}

func dispatch(cmd string, args []string, env *Env, screen *ui.Screen) error {
	switch cmd {
	case "":
		return menu(env, screen)
	case "version":
		fmt.Fprintf(env.Stdout, "DockHop %s（%s）\n", env.Version, env.Platform)
		return nil
	case "out":
		opts, err := parseOut(args)
		if err != nil {
			return err
		}
		return runOut(env, screen, opts)
	case "in":
		opts, err := parseIn(args, false)
		if err != nil {
			return err
		}
		return runIn(env, screen, opts, false)
	case "check":
		opts, err := parseIn(args, true)
		if err != nil {
			return err
		}
		return runIn(env, screen, opts, true)
	case "diagnose":
		if len(args) > 0 {
			return usageErr("diagnose 不需要参数")
		}
		return runDiagnose(env, screen)
	default:
		if strings.HasPrefix(cmd, "-") {
			return usageErr("不认识的选项 %s，运行 dockhop --help 查看用法", cmd)
		}
		return usageErr("不认识的命令 %s。迁出用 dockhop out，迁入用 dockhop in '链接'，运行 dockhop --help 查看全部用法", cmd)
	}
}

func menu(env *Env, screen *ui.Screen) error {
	p := ui.NewPrompter(env.Input, env.Stderr)
	if !p.Interactive() {
		fmt.Fprint(env.Stdout, helpText)
		return usageErr("没有可以输入的终端，请直接写明命令，例如 dockhop out 或 dockhop in '链接'")
	}
	screen.Println("DockHop " + env.Version + "：一条链接，把 Docker 服务连同数据迁到新服务器。\n")
	choice, err := p.Choose([]string{
		"迁出：打包这台服务器上的服务，生成迁移链接（在旧服务器上运行）",
		"迁入：将服务迁到这台服务器（在新服务器上运行）",
		"收集诊断信息",
	}, 1)
	if err != nil {
		return err
	}
	switch choice {
	case 1:
		return runOut(env, screen, OutOptions{Snapshot: "auto", Port: 8080})
	case 2:
		return runIn(env, screen, InOptions{Existing: "replace", HealthTimeout: defaultHealthTimeout}, false)
	default:
		return runDiagnose(env, screen)
	}
}

// errNotReady 是还没开发完的功能。
var errNotReady = errors.New("还在开发中")

func runOut(env *Env, _ *ui.Screen, opts OutOptions) error {
	t, err := startTask(env, "out")
	if err != nil {
		return err
	}
	if err := backup.RecoverInterrupted(env.Context, t.docker, t.home, t.screen, t.prompt); err != nil {
		t.screen.Error(err.Error(), "")
		t.finish(false)
		return errReported
	}
	err = backup.Run(env.Context, backup.Deps{
		Docker: t.docker, Screen: t.screen, Prompt: t.prompt, Log: t.log, Redactor: t.redactor,
		Home: t.home, Version: env.Version, Getenv: env.Getenv,
	}, backup.Options{
		Include: opts.Include, NoStop: opts.NoStop, StopShared: opts.StopShared, Snapshot: opts.Snapshot,
		Port: opts.Port, Host: opts.Host, Workdir: opts.Workdir,
	})
	t.finish(err == nil)
	if errors.Is(err, backup.ErrReported) {
		return errReported
	}
	return err
}

// readLink 取得迁移链接：命令里写了就用，否则在终端里请用户粘贴。
func readLink(env *Env, given string) (crypto.Link, error) {
	if given != "" {
		return crypto.ParseLink(given)
	}
	p := ui.NewPrompter(env.Input, env.Stderr)
	if !p.Interactive() {
		return crypto.Link{}, usageErr("请在命令后面写上迁移链接，例如 dockhop in '链接'")
	}
	var link crypto.Link
	_, err := p.Ask("请粘贴旧服务器上显示的迁移链接：", func(s string) error {
		l, err := crypto.ParseLink(s)
		if err != nil {
			return err
		}
		link = l
		return nil
	})
	return link, err
}

func runIn(env *Env, screen *ui.Screen, opts InOptions, checkOnly bool) error {
	link, err := readLink(env, opts.Link)
	if err != nil {
		var le *crypto.LinkError
		if errors.As(err, &le) || errors.Is(err, crypto.ErrOldLink) {
			screen.Error(err.Error(), "")
			return errReported
		}
		return err
	}
	_ = link
	name := "迁入"
	if checkOnly {
		name = "迁入前检查"
	}
	screen.Error(name+"功能还在开发中", "现在请先使用 docker_migrate：https://github.com/lx969788249/docker_migrate")
	return errNotReady
}

// errReported 表示错误已经显示过了，只需要返回失败的退出码。
var errReported = errors.New("已显示")

func exitCode(err error, stderr io.Writer) int {
	if err == nil {
		return ExitOK
	}
	var ue *UsageError
	switch {
	case errors.As(err, &ue):
		fmt.Fprintln(stderr, "✗ "+ue.Msg)
		return ExitUsage
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(stderr, "已中断。")
		return ExitInterrupted
	case errors.Is(err, ui.ErrNotInteractive):
		fmt.Fprintln(stderr, "✗ 需要回答问题，但没有可以输入的终端")
		return ExitUsage
	case errors.Is(err, io.EOF):
		fmt.Fprintln(stderr, "\n已取消。")
		return ExitInterrupted
	case errors.Is(err, errReported), errors.Is(err, errNotReady), errors.Is(err, flag.ErrHelp):
		return ExitFailed
	default:
		fmt.Fprintln(stderr, "✗ "+err.Error())
		return ExitFailed
	}
}
