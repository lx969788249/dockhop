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
	t.screen = env.screen
	t.screen.SetLogger(t.log)
	t.prompt = env.prompt
	dc, err := docker.New(env.Context, docker.WithLogger(t.log.Printf))
	if err != nil {
		reason, hint := explainDockerError(err)
		t.screen.Problem(reason, hint)
		t.finish(ui.ErrReported)
		return nil, ui.ErrReported
	}
	t.docker = dc
	// 出问题时用户只需附上日志：Docker 的情况也记下来。
	if info, err := dc.Info(env.Context); err == nil {
		t.log.Printf("Docker %s · %s · %s · 存储 %s · cgroup v%s · %d 核 · 内存 %s · 数据目录 %s", info.ServerVersion,
			info.OperatingSystem, info.Architecture, info.Driver, info.CgroupVersion, info.NCPU,
			ui.HumanBytes(info.MemTotal), info.DockerRootDir)
	}
	return t, nil
}

// finish 结束任务：按结局处理日志（见 logging.Outcome；测试时 DOCKHOP_TEST_KEEP_LOG=1 都保留），
// 再删掉 DockHop 目录里空了的子目录，目录整个空了就把它也删掉。迁移成功、只是看看之后，
// 服务器上不留东西；失败时留下的日志、下载照旧留着。
func (t *task) finish(err error) {
	outcome := logging.Failed
	switch {
	case err == nil:
		outcome = logging.Succeeded
	case errors.Is(err, ui.ErrCancelled): // 还没改动就取消了
		outcome = logging.Cancelled
	}
	t.log.Finish(outcome, t.env.Getenv("DOCKHOP_TEST_KEEP_LOG") == "1")
	tidyHome(t.home)
}

// tidyHome 删掉 home 里空了的子目录（DockHop 自己建的这几个），home 空了也删掉。只删空目录，
// 有内容的不动。
func tidyHome(home string) {
	for _, sub := range []string{"logs", "work", "state", "rollback", "bin", "locks"} {
		_ = os.Remove(filepath.Join(home, sub))
	}
	_ = os.Remove(home)
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
