package restore

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/compose"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/docker/dockertest"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 猜出来的依赖等不到时不拦着：接着启动用到它的，它最后再等一次（完整的时间再多 1 分钟）。
// Docker 要求的依赖等不到时，用到它的不启动。
func TestWaitBeforeSoftAndHard(t *testing.T) {
	_, _, r := fakeContainer(t, "", `{"Id":"new-app","Name":"/app","State":{"Status":"exited","ExitCode":1}}`)
	dep := &unit{key: "c:app", label: "容器 app", started: true,
		items: []*citem{{m: bundle.Container{Name: "app"}, newID: "new-app", c: &docker.Container{}, startedAt: time.Now()}}}
	dep.items[0].c.State.Running = true
	user := &unit{key: "c:user", label: "容器 user", deps: []*unitDep{{on: dep, prefer: true, why: []string{"猜的"}}}}
	if err := r.waitBefore(context.Background(), user); err != nil {
		t.Fatalf("猜出来的依赖等不到时不应拦着：%v", err)
	}
	if !dep.waited || !dep.items[0].startedAt.IsZero() || dep.items[0].extra != time.Minute {
		t.Errorf("它最后应再等一次，多给 1 分钟：waited=%v startedAt=%v extra=%v", dep.waited, dep.items[0].startedAt, dep.items[0].extra)
	}
	// 已经等过一次的，后面别的单位不再为它等。
	other := &unit{key: "c:other", deps: []*unitDep{{on: dep, prefer: true}}}
	if err := r.waitBefore(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	hard := &unit{key: "c:hard", deps: []*unitDep{{on: dep, exist: true, ready: true}}}
	if err := r.waitBefore(context.Background(), hard); err == nil {
		t.Error("Docker 要求的依赖没就绪时应失败")
	}
}

// 同一个服务一个副本在运行、一个停着：docker compose start web 会把两个都启动，所以要按
// 容器只启动原来在运行的那个；它依赖的 db 先启动、就绪后再启动它。
func TestStartProjectKeepsReplicaStates(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	os.MkdirAll(bin, 0o700)
	cmdlog := filepath.Join(home, "compose.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_COMPOSE_LOG", cmdlog)
	os.WriteFile(filepath.Join(bin, "docker"), []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_COMPOSE_LOG"
case " $* " in
 *" version "*) echo 2.38.0 ;;
 *" config "*) echo '{"services":{"web":{"image":"alpine","scale":2,"depends_on":{"db":{"condition":"service_healthy"}}},"db":{"image":"alpine"}}}' ;;
esac
`), 0o700)
	tool, err := compose.Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var started []string
	fake := dockertest.New(t, func(w http.ResponseWriter, req *http.Request) bool {
		p := req.URL.Path
		switch {
		case p == "/networks":
			io.WriteString(w, `[]`)
		case p == "/containers/json":
			io.WriteString(w, `[{"Id":"new-web-1","Labels":{"com.docker.compose.service":"web","com.docker.compose.container-number":"1"}},`+
				`{"Id":"new-web-2","Labels":{"com.docker.compose.service":"web","com.docker.compose.container-number":"2"}},`+
				`{"Id":"new-db-1","Labels":{"com.docker.compose.service":"db","com.docker.compose.container-number":"1"}}]`)
		case strings.HasSuffix(p, "/start"):
			mu.Lock()
			started = append(started, strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/start"))
			mu.Unlock()
			w.WriteHeader(204)
		case strings.HasSuffix(p, "/json"):
			io.WriteString(w, `{"Id":"x","State":{"Status":"running","Running":true},"Config":{"Image":"alpine"}}`)
		default:
			return false
		}
		return true
	})
	dc := fake.Client()
	member := func(name, svc, number string, running bool) *citem {
		c := &docker.Container{}
		c.Config.Image = "alpine"
		c.Config.Labels = map[string]string{"com.docker.compose.container-number": number}
		c.State.Running = running
		return &citem{m: bundle.Container{Name: name, Project: "stack", Service: svc}, c: c}
	}
	j, err := openJournal(filepath.Join(home, "rollback"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.close()
	var out bytes.Buffer
	r := testRun(t, Deps{Docker: dc, Screen: ui.New(&out, false, nil), Home: home, Getenv: func(k string) string {
		if k == "DOCKHOP_TEST_STARTUP_GRACE" {
			return "0s"
		}
		return ""
	}}, Options{})
	r.tool = tool
	r.j = j
	r.workdir = home
	r.items = []*citem{
		member("stack-web-1", "web", "1", true), member("stack-web-2", "web", "2", false), member("stack-db-1", "db", "1", true)}
	u := &unit{key: "p:stack", project: &bundle.Project{Name: "stack", WorkingDir: home}, items: r.items}
	if err := r.createProject(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if err := r.startProject(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(cmdlog)
	if strings.Join(started, ",") != "new-db-1,new-web-1" || strings.Contains(string(log), " start") {
		t.Fatalf("启动了 %v；compose 命令：\n%s", started, log)
	}
}
