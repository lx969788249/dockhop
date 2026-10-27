package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/logging"
	"github.com/lx969788249/dockhop/internal/ui"
)

// task 是迁出、迁入共用的准备工作：日志、屏幕、提问、Docker 连接。
type task struct {
	env      *Env
	home     string
	log      *logging.Logger
	redactor *logging.Redactor
	screen   *ui.Screen
	prompt   *ui.Prompter
	docker   *docker.Client
}

func startTask(env *Env, kind string) (*task, error) {
	t := &task{env: env, home: env.dockhopHome(), redactor: &logging.Redactor{}}
	log, err := logging.Open(filepath.Join(t.home, "logs"), kind, t.redactor)
	if err != nil {
		// 没有日志也能继续，只是出错时没有日志可以反馈。
		fmt.Fprintf(env.Stderr, "    ! 无法创建日志（%v），继续运行\n", err)
	}
	t.log = log
	t.log.Block(fmt.Sprintf("===== DockHop %s · %s =====\n参数：%s\n系统：%s · %s\n用户：uid=%d\n",
		env.Version, kind, strings.Join(env.Args, " "), host.OSName(), host.Kernel(), os.Geteuid()))
	t.screen = ui.New(env.Stderr, env.TTY, t.log)
	t.screen.SetVerbose(env.verbose)
	t.screen.SetWidth(env.Width)
	t.screen.SetBanner(ui.Banner(env.Version, env.TTY))
	t.prompt = ui.NewPrompter(env.Input, env.Stderr).WithContext(env.Context)
	dc, err := docker.New(env.Context, docker.WithLogger(t.log.Printf))
	if err != nil {
		reason, hint := explainDockerError(err)
		t.screen.Error(reason, hint)
		t.finish(false)
		return nil, errReported
	}
	t.docker = dc
	return t, nil
}

// finish 结束日志：成功删除，失败保留（DOCKHOP_KEEP_LOG=1 时成功也保留）。
func (t *task) finish(success bool) {
	t.log.Finish(success, t.env.Getenv("DOCKHOP_KEEP_LOG") == "1")
}

func explainDockerError(err error) (string, string) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "permission denied"):
		return "当前用户没有访问 Docker 的权限", "请用 root 运行，或把当前用户加入 docker 组后重新登录"
	case errors.Is(err, docker.ErrNotRunning):
		return "连不上 Docker：" + msg, "确认已经安装 Docker 并且正在运行：systemctl start docker"
	default:
		return msg, ""
	}
}
