package restore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/logging"
	"github.com/lx969788249/dockhop/internal/ui"
)

func logText(t *testing.T, r *run) string {
	t.Helper()
	b, err := os.ReadFile(r.Log.Path())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const healthyAfterFailures = `{"Id":"new-app","Name":"/app","State":{"Status":"running","Running":true,"Health":{"Status":"%s","FailingStreak":%d,"Log":[%s]}},
	"Config":{"Healthcheck":{"Test":["CMD","curl","-f","http://localhost:8090/"],"Interval":30000000000,"Timeout":5000000000,"StartPeriod":30000000000,"Retries":5}}}`

// 健康检查中途没通过、显示不健康都接着等，通过了就算就绪；每次检查的结果记进日志，
// 每秒一次的查询不记。
func TestWaitReadyFollowsHealthcheck(t *testing.T) {
	fail1 := `{"Start":"2026-11-03T01:00:01Z","End":"2026-11-03T01:00:02Z","ExitCode":7,"Output":"curl: (7) Failed to connect to localhost port 8090"}`
	fail2 := `{"Start":"2026-11-03T01:00:31Z","End":"2026-11-03T01:00:32Z","ExitCode":22,"Output":"curl: (22) The requested URL returned error: 503"}`
	ok := `{"Start":"2026-11-03T01:01:01Z","End":"2026-11-03T01:01:02Z","ExitCode":0,"Output":""}`
	_, _, r := fakeContainer(t, "",
		fmt.Sprintf(healthyAfterFailures, "starting", 0, ""),
		fmt.Sprintf(healthyAfterFailures, "starting", 1, fail1),
		fmt.Sprintf(healthyAfterFailures, "unhealthy", 2, fail1+","+fail2),
		fmt.Sprintf(healthyAfterFailures, "healthy", 0, fail1+","+fail2+","+ok))
	it := &citem{m: bundle.Container{Name: "app"}, newID: "new-app", startedAt: time.Now()}
	if err := r.waitReady(context.Background(), it); err != nil {
		t.Fatalf("健康检查通过了应算就绪：%v", err)
	}
	text := logText(t, r)
	for _, w := range []string{
		"健康检查（启动后 30 秒内不算，之后每 30 秒查一次（每次最多 5 秒），连续 5 次没通过算不健康），最多等 3 分 55 秒",
		"容器 app 健康检查没通过（退出码 7）：curl: (7) Failed to connect to localhost port 8090",
		"容器 app 健康检查没通过（退出码 22）：curl: (22) The requested URL returned error: 503",
		"容器 app：不健康", "容器 app 健康检查通过", "容器 app 就绪：健康检查通过",
	} {
		if !strings.Contains(text, w) {
			t.Errorf("日志里缺少「%s」：\n%s", w, text)
		}
	}
	if strings.Count(text, "Failed to connect") != 1 {
		t.Errorf("同一次健康检查只记一次：\n%s", text)
	}
	if strings.Contains(text, "Docker 接口：GET") {
		t.Errorf("每秒一次的查询不应记进日志：\n%s", text)
	}
}

// 容器退出了、Docker 不再重启它：马上失败。报错里是最能说明问题的那一行（不是调用栈），
// 按名字找不到别的服务时，建议里说出是哪个名字；容器最后的输出完整记进日志。
func TestWaitReadyExitedExplains(t *testing.T) {
	logs := strings.Join([]string{
		"2026-11-03 01:10:43.123 ERROR 7 --- [main] o.s.boot.SpringApplication : Application run failed",
		"java.net.UnknownHostException: Failed to resolve 'postgresql' [A(1)] after 2 queries",
		"\tat io.netty.resolver.dns.DnsResolveContext.finishResolve(DnsResolveContext.java:1139)",
		"\t... 33 common frames omitted",
		"Caused by: io.netty.resolver.dns.DnsErrorCauseException: Query failed with NXDOMAIN",
		"\tat io.netty.resolver.dns.DnsResolveContext.onResponse(..)(Unknown Source)",
	}, "\n")
	_, _, r := fakeContainer(t, logs,
		`{"Id":"new-app","Name":"/app","RestartCount":0,"State":{"Status":"exited","ExitCode":1}}`)
	start := time.Now()
	err := r.waitReady(context.Background(), &citem{m: bundle.Container{Name: "app"}, newID: "new-app", startedAt: time.Now()})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("退出了应马上失败：%v（%s）", err, time.Since(start))
	}
	want := "容器 app 启动后马上退出了（退出码 1），最后的输出：Caused by: io.netty.resolver.dns.DnsErrorCauseException: Query failed with NXDOMAIN"
	if err.Error() != want {
		t.Errorf("报错：%s\n应为：%s", err, want)
	}
	if hint := groupHint(err); !strings.Contains(hint, "按名字找不到它要连的 postgresql") {
		t.Errorf("建议应说出找不到的名字：%s", hint)
	}
	if text := logText(t, r); !strings.Contains(text, "Failed to resolve 'postgresql'") {
		t.Errorf("容器最后的输出应完整记进日志：\n%s", text)
	}
}

// 没有健康检查的容器一直在重启：等到时间才失败（它要连的服务可能还没起来），报错里说明重启了
// 几次；每次重启记进日志。
func TestWaitReadyRestartLoop(t *testing.T) {
	minHealthWait = 3 * time.Second
	t.Cleanup(func() { minHealthWait = 60 * time.Second })
	var states []string
	for i := 1; i <= 6; i++ {
		states = append(states, fmt.Sprintf(`{"Id":"new-app","Name":"/app","RestartCount":%d,"State":{"Status":"restarting","ExitCode":1}}`, i))
	}
	_, _, r := fakeContainer(t, "nc: bad address 'db'", states...)
	err := r.waitReady(context.Background(), &citem{m: bundle.Container{Name: "app"}, newID: "new-app", startedAt: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "没能稳定运行（等了 3 秒，最后的状态：正在重启，自动重启了") ||
		!strings.Contains(err.Error(), "最后的输出：nc: bad address 'db'") {
		t.Fatalf("报错：%v", err)
	}
	if text := logText(t, r); !strings.Contains(text, "容器 app 退出了（退出码 1），Docker 正在自动重启它（第 2 次）") {
		t.Errorf("日志里应记下重启：\n%s", text)
	}
}

// 稳定运行就算就绪：Docker 记的启动时间已经过了等待的时间，不用再等。
func TestWaitReadyStableRunning(t *testing.T) {
	started := time.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339Nano)
	_, _, r := fakeContainer(t, "", `{"Id":"new-app","Name":"/app","State":{"Status":"running","Running":true,"StartedAt":"`+started+`"}}`)
	start := time.Now()
	if err := r.waitReady(context.Background(), &citem{m: bundle.Container{Name: "app"}, newID: "new-app"}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("已经稳定运行了，不用再等：%s", time.Since(start))
	}
}

// 没有健康检查的容器稳定运行几秒就算就绪，之后还可能停掉。决定提交之前把就绪了的容器再查一遍：
// 停了、在重启、不健康了的，它所在的组按没能启动处理；还在运行的不受影响。
func TestConfirmReady(t *testing.T) {
	for _, c := range []struct{ state, want string }{
		{`{"Id":"new-app","Name":"/app","State":{"Status":"exited","ExitCode":1}}`, "容器 app 就绪之后又停了（退出码 1），最后的输出：panic: boom"},
		{`{"Id":"new-app","Name":"/app","State":{"Status":"restarting","Running":true,"ExitCode":2}}`, "容器 app 就绪之后又退出了，Docker 正在重启它（退出码 2），最后的输出：panic: boom"},
		{`{"Id":"new-app","Name":"/app","State":{"Status":"running","Running":true,"Health":{"Status":"unhealthy"}}}`, "容器 app 就绪之后健康检查又没通过，最后的输出：panic: boom"},
		{`{"Id":"new-app","Name":"/app","State":{"Status":"running","Running":true,"Health":{"Status":"healthy"}}}`, ""},
		{`{"Id":"new-app","Name":"/app","State":{"Status":"paused","Running":true,"Paused":true}}`, ""},
	} {
		_, _, r := fakeContainer(t, "panic: boom", c.state)
		src := &docker.Container{}
		src.State.Running = true
		g := &group{id: "g", items: []*citem{{m: bundle.Container{Name: "app"}, c: src, newID: "new-app", ready: true}}}
		r.groups = []*group{g}
		if err := r.confirmReady(context.Background()); err != nil {
			t.Fatal(err)
		}
		if g.reason != c.want {
			t.Errorf("%s\n  原因：%q\n  应为：%q", c.state, g.reason, c.want)
		}
	}
}

func TestKeyLine(t *testing.T) {
	cases := []struct{ logs, want string }{
		{"Starting...\nListening on :8080", "Listening on :8080"},
		{"Error: getaddrinfo ENOTFOUND db\n    at GetAddrInfoReqWrap.onlookup [as oncomplete] (node:dns:107:26)", "Error: getaddrinfo ENOTFOUND db"},
		{"panic: dial tcp: lookup db on 127.0.0.11:53: no such host\n\ngoroutine 1 [running]:\nmain.main()\n\t/app/main.go:12 +0x1d",
			"panic: dial tcp: lookup db on 127.0.0.11:53: no such host"},
		{"Traceback (most recent call last):\n  File \"/app/main.py\", line 3, in <module>\n    connect()\nConnectionRefusedError: [Errno 111] Connection refused",
			"ConnectionRefusedError: [Errno 111] Connection refused"},
		{"", ""},
	}
	for _, c := range cases {
		if got := keyLine(c.logs); got != c.want {
			t.Errorf("keyLine(%q) = %q，应为 %q", c.logs, got, c.want)
		}
	}
	if got := keyLine(strings.Repeat("错", 300)); len([]rune(got)) != 161 {
		t.Errorf("太长时截断：%d", len([]rune(got)))
	}
}

func TestUnresolvedHost(t *testing.T) {
	cases := map[string]string{
		"java.net.UnknownHostException: Failed to resolve 'postgresql' [A(1)] after 2 queries": "postgresql",
		"java.net.UnknownHostException: mysql: Name or service not known":                      "mysql",
		"Error: getaddrinfo ENOTFOUND redis":                                                   "redis",
		`psql: error: could not translate host name "db" to address: Name does not resolve`:    "db",
		"dial tcp: lookup gitea-db on 127.0.0.11:53: no such host":                             "gitea-db",
		"nc: bad address 'dmtest_1Panel-postgresql-AbCd'":                                      "dmtest_1Panel-postgresql-AbCd",
		"ERROR 2005 (HY000): Unknown MySQL server host 'mariadb' (-2)":                         "mariadb",
		"php_network_getaddresses: getaddrinfo for mysql failed: Name or service not known":    "mysql",
		"Connection refused": "",
		"bad address 'old'\nlater: bad address 'newest-name'": "newest-name",
	}
	for logs, want := range cases {
		if got := unresolvedHost(logs); got != want {
			t.Errorf("unresolvedHost(%q) = %q，应为 %q", logs, got, want)
		}
	}
	if h := unresolvedHint("halo", "Failed to resolve 'api.example.com'"); !strings.Contains(h, "解析不了域名 api.example.com") {
		t.Errorf("外面的域名：%s", h)
	}
}

func TestHealthTimeout(t *testing.T) {
	s := int64(time.Second)
	if got := healthTimeout(60*time.Second, 0, 2*s, 2*s, 1); got != 60*time.Second {
		t.Errorf("检查得勤时至少等 60 秒：%s", got)
	}
	// 1Panel 装的 Halo：启动后 30 秒内不算，之后每 30 秒一次、每次最多 5 秒，连续 5 次没通过才算
	// 不健康。Docker 最晚 30 + 5 ×（30 + 5）= 205 秒下结论，再多等 30 秒。以前只等 95 秒。
	if got := healthTimeout(60*time.Second, 30*s, 30*s, 5*s, 5); got != 235*time.Second {
		t.Errorf("应等到 Docker 下结论再多 30 秒：%s", got)
	}
	// 每次检查都要用满时间（检查很慢）时，每一轮都要算上：30 + 5 ×（30 + 30）+ 30 = 360 秒。
	if got := healthTimeout(60*time.Second, 30*s, 30*s, 30*s, 5); got != 360*time.Second {
		t.Errorf("每一轮检查的时间都要算上：%s", got)
	}
	// Vaultwarden：每 60 秒一次、每次最多 10 秒，没写次数（Docker 默认 3 次）。
	if got := healthTimeout(60*time.Second, 0, 60*s, 10*s, 0); got != 240*time.Second {
		t.Errorf("没写的按 Docker 的默认值：%s", got)
	}
	if got := healthTimeout(60*time.Second, 2000*s, 2000*s, 0, 3); got != time.Hour {
		t.Errorf("最多一小时：%s", got)
	}
}

// 迁入的容器启动失败时，它最后的输出会写进日志、接在报错后面。输出里带着环境变量里的
// 连接串时，两处都要隐藏（报错会显示在结果里，用户可能整段复制去求助）。
func TestStartupFailureHidesEnvSecrets(t *testing.T) {
	const secret = "postgres://app:only-for-test@db/app"
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
		switch req.URL.Path {
		case "/v1.44/containers/json":
			io.WriteString(w, `[]`)
		case "/v1.44/containers/new-web/json":
			io.WriteString(w, `{"Id":"new-web","Name":"/web","State":{"Status":"exited"}}`)
		case "/v1.44/containers/new-web/logs":
			io.WriteString(w, "cannot connect using "+secret)
		default:
			return false
		}
		return true
	})
	root := t.TempDir()
	data := filepath.Join(root, "data")
	os.MkdirAll(filepath.Join(data, "containers"), 0o700)
	os.WriteFile(filepath.Join(data, "containers", "web.json"),
		[]byte(`{"Id":"old-web","Name":"/web","Config":{"Image":"alpine","Env":["DATABASE_URL=`+secret+`"]}}`), 0o600)
	rd := &logging.Redactor{}
	log, err := logging.Open(filepath.Join(root, "logs"), "in", rd)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Finish(logging.Failed, true)
	var out bytes.Buffer
	r := &run{Deps: Deps{Docker: dc, Screen: ui.New(&out, false, nil), Log: log, Redactor: rd, Home: root, Getenv: func(string) string { return "" }},
		dataDir: data, rbDir: filepath.Join(root, "rollback"), undo: &undo{}, anonVolumes: map[string]bundle.Volume{},
		plan: &bundle.Plan{Containers: []bundle.PlanContainer{{Name: "web"}}},
		m:    &bundle.Manifest{Containers: []bundle.Container{{Name: "web", Image: "alpine", State: "running"}}}}
	if err := r.decide(context.Background()); err != nil {
		t.Fatal(err)
	}
	readyErr := r.waitReady(context.Background(), &citem{m: bundle.Container{Name: "web"}, newID: "new-web"})
	if readyErr == nil || !strings.Contains(readyErr.Error(), "cannot connect using") {
		t.Fatalf("报错里应带着容器最后的输出：%v", readyErr)
	}
	logText, _ := os.ReadFile(log.Path())
	if strings.Contains(readyErr.Error(), "only-for-test") || strings.Contains(string(logText), "only-for-test") {
		t.Fatalf("连接串没有隐藏：报错 %q\n日志 %s", readyErr, logText)
	}
}
