// DockHop：一条链接，把 Docker 服务连同数据从旧服务器迁到新服务器。
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/lx969788249/dockhop/internal/cli"
)

// version 在发布时由编译参数写入。
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	tty := term.IsTerminal(int(os.Stderr.Fd()))
	wd, _ := os.Getwd()
	code := cli.Run(os.Args[1:], cli.Env{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		TTY:     tty,
		Input:   terminalInput(tty),
		Version: version,
		Workdir: wd,
		Context: ctx,
		Width:   terminalWidth,
	})
	os.Exit(code)
}

// terminalWidth 是屏幕（标准错误）有多少列，每次刷新进度时读取，窗口拉宽拉窄都跟得上。
func terminalWidth() int {
	w, _, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil {
		return 0
	}
	return w
}

// terminalInput 返回可以提问的终端输入。通过 bash <(curl …) 运行时标准输入
// 就是终端；标准输入被重定向但屏幕是终端时，改从 /dev/tty 读。都不行时返回 nil，
// 表示不能提问。
func terminalInput(stderrTTY bool) io.Reader {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return os.Stdin
	}
	if !stderrTTY {
		return nil
	}
	if f, err := os.Open("/dev/tty"); err == nil {
		return f
	}
	return nil
}
