// DockHop：只需两步，完整迁移你的 Docker。
package main

import (
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
	// Ctrl+C（以及 SIGTERM、SIGHUP）交给 cli：平时让手上的事停下，收尾时先提醒、再按一次才立即结束。
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	tty := term.IsTerminal(int(os.Stderr.Fd()))
	code := cli.Run(os.Args[1:], cli.Env{
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		TTY:        tty,
		Input:      terminalInput(tty),
		Version:    version,
		Interrupts: sigs,
		Width:      terminalWidth,
		Count:      "https://dockhop.run/count",
		Failed:     "https://dockhop.run/failed",
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
