package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/docker/dockertest"
	"github.com/lx969788249/dockhop/internal/ui"
)

type run struct {
	code   int
	stdout string
	stderr string
}

func runCLI(t *testing.T, input string, env map[string]string, args ...string) run {
	t.Helper()
	var out, errb bytes.Buffer
	e := Env{
		Stdout:   &out,
		Stderr:   &errb,
		Version:  "1.0.0-test",
		Platform: "linux/amd64",
		Getenv:   func(k string) string { return env[k] },
		HomeDir:  func() (string, error) { return t.TempDir(), nil },
		Context:  context.Background(),
	}
	if input != "" {
		e.Input = strings.NewReader(input)
	}
	code := Run(args, e)
	return run{code, out.String(), errb.String()}
}

func TestHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"in", "--help"}} {
		r := runCLI(t, "", nil, args...)
		if r.code != ExitOK || r.stdout != helpText {
			t.Errorf("%v 应显示帮助：%d %q", args, r.code, r.stderr)
		}
	}
	r := runCLI(t, "", nil, "version")
	if r.code != ExitOK || r.stdout != "DockHop 1.0.0-test（linux/amd64）\n" {
		t.Errorf("version：%d %q", r.code, r.stdout)
	}
	if r := runCLI(t, "", nil, "--version"); r.stdout != "DockHop 1.0.0-test（linux/amd64）\n" {
		t.Errorf("--version：%q", r.stdout)
	}
}

func TestUsageErrors(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"fly"}, "不认识的命令 fly"},
		{[]string{"--bogus"}, "不认识的选项 --bogus"},
		{[]string{"out", "--bogus"}, "不认识的选项 --bogus"},
		{[]string{"out", "--port", "abc"}, "选项 --port 的值不对：abc"},
		{[]string{"out", "--port", "0"}, "1 到 65535"},
		{[]string{"out", "--include"}, "--include 后面需要写一个值"},
		{[]string{"out", "--include", " , "}, "需要写容器名"},
		{[]string{"out", "web"}, "迁出不需要参数"},
		{[]string{"in", "a", "b"}, "只需要一个迁移链接"},
		{[]string{"in", "--existing", "merge", "x"}, "replace、skip 或 fail"},
		{[]string{"in", "--yes=maybe"}, "选项 --yes 的值不对"},
		{[]string{"check", "x"}, "不认识的命令 check"}, // 已经去掉的命令
		{[]string{"diagnose"}, "不认识的命令 diagnose"},
		{[]string{"in", "--file", "f", "x"}, "不认识的选项 --file"},
		{[]string{"out", "--no-stop"}, "不认识的选项 --no-stop"},
		{[]string{"in"}, "请在命令后面写上迁移链接"}, // 没有终端可以粘贴
		{[]string{}, "没有可以输入的终端"},
	}
	for _, c := range cases {
		r := runCLI(t, "", nil, c.args...)
		if r.code != ExitUsage || !strings.Contains(r.stderr, c.want) {
			t.Errorf("%v：应以 2 退出并提示「%s」，实际 %d %q", c.args, c.want, r.code, r.stderr)
		}
	}
}

func TestParseOptions(t *testing.T) {
	o, err := parseOut([]string{"--include", "web, db", "--port=9000", "--stop-shared", "-host", "1.2.3.4"})
	if err != nil || strings.Join(o.Include, "|") != "web|db" || o.Port != 9000 || !o.StopShared || o.Host != "1.2.3.4" {
		t.Fatalf("parseOut：%+v %v", o, err)
	}
	// 选项和链接可以前后穿插。
	i, err := parseIn([]string{"--yes", "LINK", "--existing", "skip", "--keep-succeeded"})
	if err != nil || i.Link != "LINK" || !i.Yes || i.Existing != "skip" || !i.KeepSucceeded {
		t.Fatalf("parseIn：%+v %v", i, err)
	}
	d, _ := parseIn(nil)
	if d.Existing != "" || d.Yes || d.KeepSucceeded {
		t.Fatalf("默认值不对：%+v", d)
	}
}

func TestInLinkMessages(t *testing.T) {
	r := runCLI(t, "", nil, "in", "https://1.2.3.4:8080/abcdefghijkl/")
	if r.code != ExitFailed || !strings.Contains(r.stderr, "缺少 # 后面的密钥") {
		t.Errorf("缺密钥：%d %q", r.code, r.stderr)
	}
	// 交互时：第一次粘贴的链接不完整会提示重新粘贴，第二次完整的链接被接受、开始迁入。
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	l, _ := crypto.NewLink("1.2.3.4", 8080)
	r = runCLI(t, "https://1.2.3.4:8080/abcdefghijkl/\n"+l.String()+"\n", nil, "in")
	if !strings.Contains(r.stderr, "缺少 # 后面的密钥") || !strings.Contains(r.stderr, "连不上 Docker") {
		t.Errorf("交互粘贴链接：%q", r.stderr)
	}
}

func TestMenu(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	r := runCLI(t, "1\n", nil)
	if !strings.Contains(r.stderr, " [1]  迁出\n [2]  迁入\n 请输入数字：") || strings.Contains(r.stderr, "[3]") {
		t.Fatalf("菜单文字不对：%q", r.stderr)
	}
	if r.code != ExitFailed || !strings.Contains(r.stderr, "连不上 Docker") || !strings.Contains(r.stderr, "systemctl start docker") {
		t.Fatalf("选迁出、连不上 Docker 时应说明原因：%d %q", r.code, r.stderr)
	}
}

// 在终端里运行时，开始前清屏；选完菜单再清一次，屏幕上不留菜单。
func TestMenuClearsScreen(t *testing.T) {
	const clearScreen = "\033[H\033[2J"
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	var errb bytes.Buffer
	Run(nil, Env{Stderr: &errb, Stdout: &bytes.Buffer{}, TTY: true, Input: strings.NewReader("1\n"), Version: "1.0.0-test",
		Getenv: func(string) string { return "" }, HomeDir: func() (string, error) { return t.TempDir(), nil }})
	out := errb.String()
	if !strings.HasPrefix(out, clearScreen) || strings.Count(out, clearScreen) != 2 {
		t.Fatalf("开始时和选完菜单后应各清屏一次：%q", out)
	}
	if after := out[strings.LastIndex(out, clearScreen):]; strings.Contains(after, "[1]  迁出") {
		t.Errorf("选完之后屏幕上不应再有菜单：%q", after)
	}
}

// localDocker 启动一个本机上的假 Docker（见 dockertest，监听 Unix 套接字），返回它的地址（DOCKER_HOST）：
// 上面有 Compose 项目 site 的容器 web，以及它的网络和数据卷。
func localDocker(t *testing.T) string {
	t.Helper()
	fake := dockertest.New(t, dockertest.NotFound, dockertest.API("1.53"), dockertest.Socket())
	fake.Reply("GET /info", `{"ServerVersion":"29.2.1","OperatingSystem":"Debian","Architecture":"x86_64","NCPU":2,"MemTotal":2147483648,"Driver":"overlay2","DockerRootDir":"/","CgroupVersion":"2","Runtimes":{"runc":{}},"DefaultRuntime":"runc","Swarm":{"LocalNodeState":"inactive"}}`)
	fake.Reply("GET /containers/json", `[{"Id":"0123456789abcdef","Names":["/web"],"Image":"nginx","State":"running","Status":"Up 2 hours","Labels":{"com.docker.compose.project":"site"},"Ports":[{"IP":"0.0.0.0","PrivatePort":80,"PublicPort":8080,"Type":"tcp"}]}]`)
	fake.Reply("GET /networks", `[{"Name":"site_default","Driver":"bridge","IPAM":{"Config":[{"Subnet":"172.18.0.0/16"}]}}]`)
	fake.Reply("GET /volumes", `{"Volumes":[{"Name":"site_data","Driver":"local","Labels":{"com.docker.compose.project":"site"}}]}`)
	return fake.Host()
}

// 失败时保留的日志里要有系统和 Docker 的情况：反馈问题时附上日志就够了。
func TestFailureLogRecordsEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DOCKER_HOST", localDocker(t))
	r := runCLI(t, "", map[string]string{"DOCKHOP_HOME": home}, "out", "--include", "nosuch")
	if r.code != ExitFailed {
		t.Fatalf("应失败：%d %q", r.code, r.stderr)
	}
	logs, _ := filepath.Glob(filepath.Join(home, "logs", "out-*.log"))
	if len(logs) != 1 {
		t.Fatalf("失败时应保留日志：%v", logs)
	}
	b, _ := os.ReadFile(logs[0])
	for _, want := range []string{"===== DockHop 1.0.0-test · out =====", "Docker 29.2.1 · Debian · x86_64 · 存储 overlay2 · cgroup v2 · 2 核"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("日志里缺少「%s」：\n%s", want, b)
		}
	}
}

// 取不到用户的主目录时不能退回 /tmp：谁都能先在那里放一个软链接，以 root 运行的 DockHop 会跟着它
// 改别的文件的权限。HOME 没设置时按用户信息找，再找不到就停下说明。
func TestDockhopHomeNotTmp(t *testing.T) {
	e := &Env{Getenv: func(string) string { return "" }, HomeDir: func() (string, error) { return "", fmt.Errorf("$HOME is not defined") }}
	if h, _, err := e.dockhopHome(); err == nil || strings.HasPrefix(h, os.TempDir()) || !strings.Contains(err.Error(), "$HOME is not defined") {
		t.Fatalf("取不到主目录时应停下，说明原因：%q %v", h, err)
	}
	// HOME 写的是相对路径时也要换成绝对路径：锁文件的路径记在临时容器的标签上，从别的目录运行也要认得出。
	e.HomeDir = func() (string, error) { return "relhome", nil }
	if h, own, err := e.dockhopHome(); err != nil || !filepath.IsAbs(h) || !own {
		t.Fatalf("应是绝对路径，是 DockHop 自己的目录：%q %v %v", h, own, err)
	}
	// 用 DOCKHOP_HOME 指定的是用户的目录（见 tidyHome）。
	e.Getenv = func(k string) string {
		if k == "DOCKHOP_HOME" {
			return "/data/dockhop"
		}
		return ""
	}
	if h, own, err := e.dockhopHome(); err != nil || h != "/data/dockhop" || own {
		t.Fatalf("DOCKHOP_HOME 指定的目录是用户的：%q %v %v", h, own, err)
	}
	t.Setenv("HOME", "")
	if h, err := userHome(); err != nil || h == "" {
		t.Fatalf("HOME 没设置时应按用户信息找：%q %v", h, err)
	}
}

// Ctrl+C（问题表 D18）：平时让手上的事停下（已经改动过的接着收尾）；收尾时（见 ui.Screen.Finish）按，先提醒
// 现在停止可能留下没收拾完的东西、确定就再按一次，提醒之后再按才立即结束。平时连按两下只是停下、提醒，
// 第三下才结束：第一下引起的收尾要做完。
func TestInterrupts(t *testing.T) {
	for _, c := range []struct {
		name      string
		finishing bool
		presses   int
		reminded  bool
		exited    bool
	}{
		{"平时按一下", false, 1, false, false},
		{"平时连按两下", false, 2, true, false},
		{"平时连按三下", false, 3, true, true},
		{"收尾时按一下", true, 1, true, false},
		{"收尾时按两下", true, 2, true, true},
	} {
		var out bytes.Buffer
		sigs := make(chan os.Signal)
		code := -1
		ctx, cancel := context.WithCancel(context.Background())
		e := &Env{Context: ctx, Interrupts: sigs, Exit: func(c int) { code = c }}
		e.screen = ui.New(&out, false, nil)
		release, started := make(chan struct{}), make(chan struct{})
		if c.finishing {
			go e.screen.Finish(ctx, func(context.Context) { close(started); <-release })
			<-started
		}
		done := make(chan struct{})
		go func() { e.watchInterrupts(cancel); close(done) }()
		for range c.presses {
			sigs <- os.Interrupt
		}
		close(sigs)
		<-done
		close(release)
		reminded := strings.Contains(out.String(), "再按一次 Ctrl+C")
		if ctx.Err() == nil || reminded != c.reminded || (code == ExitInterrupted) != c.exited {
			t.Errorf("%s：取消了 %v，提醒了 %v，立即结束了 %v\n%s", c.name, ctx.Err() != nil, reminded, code == ExitInterrupted, out.String())
		}
	}
}
