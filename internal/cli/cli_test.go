package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lx969788249/dockhop/internal/crypto"
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
		Workdir:  t.TempDir(),
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
		{[]string{"out", "--snapshot", "sometimes"}, "auto、always 或 never"},
		{[]string{"out", "--include"}, "--include 后面需要写一个值"},
		{[]string{"out", "--include", " , "}, "需要写容器名"},
		{[]string{"out", "web"}, "迁出不需要参数"},
		{[]string{"out", "--no-stop", "--stop-shared"}, "不能一起用"},
		{[]string{"in", "a", "b"}, "只需要一个迁移链接"},
		{[]string{"in", "--existing", "merge", "x"}, "replace、skip 或 fail"},
		{[]string{"in", "--health-timeout", "0", "x"}, "1 到 3600"},
		{[]string{"in", "--yes=maybe"}, "选项 --yes 的值不对"},
		{[]string{"check"}, "请在命令后面写上迁移链接"},
		{[]string{"diagnose", "x"}, "diagnose 不需要参数"},
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
	o, err := parseOut([]string{"--include", "web, db", "--port=9000", "--snapshot", "never", "-host", "1.2.3.4"})
	if err != nil || strings.Join(o.Include, "|") != "web|db" || o.Port != 9000 || o.Snapshot != "never" || o.Host != "1.2.3.4" {
		t.Fatalf("parseOut：%+v %v", o, err)
	}
	// 选项和链接可以前后穿插。
	i, err := parseIn([]string{"--yes", "LINK", "--existing", "skip", "--health-timeout", "120", "--keep-files", "--keep-succeeded"}, false)
	if err != nil || i.Link != "LINK" || !i.Yes || i.Existing != "skip" || i.HealthTimeout != 120*time.Second || !i.KeepFiles || !i.KeepSucceeded {
		t.Fatalf("parseIn：%+v %v", i, err)
	}
	d, _ := parseIn(nil, false)
	if d.Existing != "" || d.HealthTimeout != 60*time.Second {
		t.Fatalf("默认值不对：%+v", d)
	}
}

func TestInLinkMessages(t *testing.T) {
	r := runCLI(t, "", nil, "in", "http://1.2.3.4:8080/x/docker_migrate_1.tar.gz.enc#sha256=00")
	if r.code != ExitFailed || !strings.Contains(r.stderr, "docker_migrate 生成的") {
		t.Errorf("旧链接：%d %q", r.code, r.stderr)
	}
	r = runCLI(t, "", nil, "in", "http://1.2.3.4:8080/abcdefghijkl/")
	if r.code != ExitFailed || !strings.Contains(r.stderr, "缺少 # 后面的密钥") {
		t.Errorf("缺密钥：%d %q", r.code, r.stderr)
	}
	// 交互时：第一次粘贴的链接不完整会提示重新粘贴，第二次完整的链接被接受、开始迁入。
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	l, _ := crypto.NewLink("1.2.3.4", 8080)
	r = runCLI(t, "http://1.2.3.4:8080/abcdefghijkl/\n"+l.String()+"\n", nil, "in")
	if !strings.Contains(r.stderr, "缺少 # 后面的密钥") || !strings.Contains(r.stderr, "连不上 Docker") {
		t.Errorf("交互粘贴链接：%q", r.stderr)
	}
}

func TestMenu(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	r := runCLI(t, "1\n", nil)
	if !strings.Contains(r.stderr, "1) 迁出：") || !strings.Contains(r.stderr, "2) 迁入：") || !strings.Contains(r.stderr, "3) 收集诊断信息：") {
		t.Fatalf("菜单文字不对：%q", r.stderr)
	}
	if r.code != ExitFailed || !strings.Contains(r.stderr, "连不上 Docker") || !strings.Contains(r.stderr, "systemctl start docker") {
		t.Fatalf("选迁出、连不上 Docker 时应说明原因：%d %q", r.code, r.stderr)
	}
}

// fakeDockerSock 在 unix socket 上模拟一个 Docker，供诊断使用。
func fakeDockerSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"Version":"29.2.1","ApiVersion":"1.53"}`)
	})
	mux.HandleFunc("/v1.53/info", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"OperatingSystem":"Debian","Architecture":"x86_64","NCPU":2,"MemTotal":2147483648,"Driver":"overlay2","DockerRootDir":"/","CgroupVersion":"2","Runtimes":{"runc":{}},"DefaultRuntime":"runc","Swarm":{"LocalNodeState":"inactive"}}`)
	})
	mux.HandleFunc("/v1.53/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"Id":"0123456789abcdef","Names":["/web"],"Image":"nginx","State":"running","Status":"Up 2 hours","Labels":{"com.docker.compose.project":"site"},"Ports":[{"IP":"0.0.0.0","PrivatePort":80,"PublicPort":8080,"Type":"tcp"}]}]`)
	})
	mux.HandleFunc("/v1.53/networks", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"Name":"site_default","Driver":"bridge","IPAM":{"Config":[{"Subnet":"172.18.0.0/16"}]}}]`)
	})
	mux.HandleFunc("/v1.53/volumes", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"Volumes":[{"Name":"site_data","Driver":"local","Labels":{"com.docker.compose.project":"site"}}]}`)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return "unix://" + sock
}

func TestDiagnose(t *testing.T) {
	home := t.TempDir()
	logs := filepath.Join(home, "logs")
	os.MkdirAll(logs, 0o700)
	os.WriteFile(filepath.Join(logs, "in-1.log"), []byte("[12:00:00] -e DB_PASSWORD=hunter2\n[12:00:01] 恢复失败\n"), 0o600)

	t.Setenv("DOCKER_HOST", fakeDockerSock(t))
	r := runCLI(t, "", map[string]string{"DOCKHOP_HOME": home}, "diagnose")
	if r.code != ExitOK {
		t.Fatalf("diagnose 失败：%d %q", r.code, r.stderr)
	}
	path := ""
	for _, l := range strings.Split(r.stderr, "\n") {
		if strings.HasPrefix(l, "诊断文件：") {
			path = strings.TrimPrefix(l, "诊断文件：")
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到诊断文件 %q：%v", path, err)
	}
	report := string(b)
	for _, want := range []string{
		"DockHop：1.0.0-test（linux/amd64）",
		"版本：29.2.1 · 接口 1.53（使用 1.53）",
		"存储：overlay2 · / · cgroup v2",
		"web · nginx · running · Up 2 hours · 端口 0.0.0.0:8080->80/tcp · Compose 项目 site",
		"site_default · bridge · 172.18.0.0/16",
		"site_data · local · Compose 项目 site",
		"===== 最近的运行日志",
		"DB_PASSWORD=***",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("诊断文件里缺少「%s」", want)
		}
	}
	if strings.Contains(report, "hunter2") {
		t.Error("诊断文件里不应出现密码")
	}

	// 连不上 Docker 时照样生成诊断文件，并写明原因。
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	r = runCLI(t, "", map[string]string{"DOCKHOP_HOME": home}, "diagnose")
	if r.code != ExitOK || !strings.Contains(r.stderr, "诊断文件：") {
		t.Fatalf("连不上 Docker 时也应生成诊断文件：%d %q", r.code, r.stderr)
	}
}

// 旧服务器显示的是一整条迁入命令，粘贴整条命令或者只粘贴链接都要认得。
func TestLinkIn(t *testing.T) {
	const link = "http://127.0.0.1:8080/k9Qm2xTa/#abc"
	for _, in := range []string{
		link,
		"  " + link + "  ",
		"bash <(curl -fsSL dockhop.run) in '" + link + "'",
		`dockhop in "` + link + `" --yes`,
	} {
		if got := linkIn(in); got != link {
			t.Errorf("linkIn(%q) = %q", in, got)
		}
	}
	if got := linkIn(" abc "); got != "abc" {
		t.Errorf("不是链接时原样交给 ParseLink 报错：%q", got)
	}
}
