package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/logging"
	"github.com/lx969788249/dockhop/internal/ui"
)

// oldContainer 按 docker inspect 的样子造一个旧服务器上的容器。
func oldContainer(t *testing.T, raw string) *citem {
	t.Helper()
	var c docker.Container
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	return &citem{m: bundle.Container{Name: c.ShortName(), Project: c.Config.Labels["com.docker.compose.project"],
		Service: c.Config.Labels["com.docker.compose.service"]}, c: &c, raw: json.RawMessage(raw)}
}

// 1Panel 装的应用是各自的 Compose 项目，接在同一个 1panel-network 上。Halo 的启动参数里写着
// 数据库的服务名 postgresql：PostgreSQL 要先启动，等它就绪后再启动 Halo；Nginx Proxy Manager
// 和它们没关系，不用等。
func TestStartOrderFollowsReferences(t *testing.T) {
	halo := oldContainer(t, `{"Name":"/halo","Config":{"Labels":{"com.docker.compose.project":"halo","com.docker.compose.service":"halo"},
		"Env":["JVM_OPTS=","PATH=/usr/local/sbin:/usr/local/bin"],
		"Cmd":["--spring.r2dbc.url=r2dbc:pool:postgresql://postgresql:5432/halo","--spring.r2dbc.password=secret-pass","--spring.sql.init.platform=postgresql"]},
		"NetworkSettings":{"Networks":{"1panel-network":{"Aliases":["halo"],"DNSNames":["halo","0123456789ab"]}}}}`)
	npm := oldContainer(t, `{"Name":"/1Panel-nginx-proxy-manager-Xy12","Config":{"Labels":{"com.docker.compose.project":"nginx-proxy-manager","com.docker.compose.service":"nginx-proxy-manager"},
		"Env":["DB_SQLITE_FILE=/data/database.sqlite"],"Cmd":null},
		"NetworkSettings":{"Networks":{"1panel-network":{"Aliases":["nginx-proxy-manager"]}}}}`)
	pg := oldContainer(t, `{"Name":"/1Panel-postgresql-Ab12","Config":{"Labels":{"com.docker.compose.project":"postgresql","com.docker.compose.service":"postgresql"},
		"Env":["POSTGRES_USER=user_ab12","POSTGRES_PASSWORD=pg-pass","PGDATA=/var/lib/postgresql/18/docker"],"Cmd":["postgres"]},
		"NetworkSettings":{"Networks":{"1panel-network":{"Aliases":["postgresql","1Panel-postgresql-Ab12"],"DNSNames":["1Panel-postgresql-Ab12","postgresql","fedcba987654"]}}}}`)
	// 独立容器：环境变量里写着 Halo 的地址，但不在同一个网络上，按名字本来就连不上，不算。
	probe := oldContainer(t, `{"Name":"/probe","Config":{"Env":["TARGET=http://halo:8090"]},"NetworkSettings":{"Networks":{"bridge":{}}}}`)
	// 独立容器：环境变量里写着数据库的容器名（大小写不同也算）。
	backup := oldContainer(t, `{"Name":"/pg-backup","Config":{"Env":["PGHOST=1panel-postgresql-ab12"]},
		"NetworkSettings":{"Networks":{"1panel-network":{"Aliases":null}}}}`)

	projects := []bundle.Project{{Name: "halo"}, {Name: "nginx-proxy-manager"}, {Name: "postgresql"}}
	r := &run{items: []*citem{halo, npm, pg, probe, backup}, projects: projects}
	units := r.units(&group{projects: projects, items: r.items})
	deps := map[string]string{}
	for _, u := range units {
		var parts []string
		for _, d := range u.deps {
			parts = append(parts, d.on.key+"（"+d.why+"）")
		}
		deps[u.key] = strings.Join(parts, "；")
	}
	want := map[string]string{
		"p:halo":                "p:postgresql（halo 的启动命令里写着 postgresql）",
		"p:nginx-proxy-manager": "",
		"p:postgresql":          "",
		"c:probe":               "",
		"c:pg-backup":           "p:postgresql（pg-backup 的环境变量 PGHOST 里写着 1panel-postgresql-ab12）",
	}
	for k, w := range want {
		if deps[k] != w {
			t.Errorf("%s 用到：%q，应为 %q", k, deps[k], w)
		}
	}
	var order []string
	for _, u := range dependencyOrder(units, unitKey, (*unit).depKeys) {
		order = append(order, u.key)
	}
	if got := strings.Join(order, " "); got != "p:nginx-proxy-manager p:postgresql c:probe c:pg-backup p:halo" {
		t.Errorf("启动顺序：%s", got)
	}
	if why := deps["p:halo"]; strings.Contains(why, "secret-pass") {
		t.Errorf("理由里不应有启动参数的值：%s", why)
	}
}

// 假 Docker 上的一个容器：每次查询按顺序给出下一个样子，最后一个样子一直保持。
func fakeContainer(t *testing.T, logs string, states ...string) (*docker.Client, *bytes.Buffer, *run) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
		switch {
		case strings.HasSuffix(req.URL.Path, "/containers/new-app/json"):
			mu.Lock()
			s := states[min(n, len(states)-1)]
			n++
			mu.Unlock()
			io.WriteString(w, s)
		case strings.HasSuffix(req.URL.Path, "/containers/new-app/logs"):
			io.WriteString(w, logs)
		default:
			return false
		}
		return true
	})
	root := t.TempDir()
	log, err := logging.Open(filepath.Join(root, "logs"), "in", &logging.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Finish(false, true) })
	var out bytes.Buffer
	r := &run{Deps: Deps{Docker: dc, Screen: ui.New(&out, false, nil), Log: log, Redactor: &logging.Redactor{},
		Getenv: func(k string) string {
			if k == "DOCKHOP_TEST_STARTUP_GRACE" {
				return "1s"
			}
			return ""
		}}}
	return dc, &out, r
}

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
		"健康检查（启动后 30 秒内不算，之后每 30 秒查一次，连续 5 次没通过算不健康），最多等 3 分 35 秒",
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
