package restore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/logging"
	"github.com/lx969788249/dockhop/internal/preflight"
	"github.com/lx969788249/dockhop/internal/ui"
)

// noMounts 是没有任何单独挂载的目录时的 Deps.MountPoint。
func noMounts(string) (bool, error) { return false, nil }

// faultyDocker 是一个假 Docker（接口版本 1.44）：handle 决定每个请求怎么回答（返回 false 时回答 500），
// 记下收到的每个请求。
func faultyDocker(t *testing.T, handle func(w http.ResponseWriter, r *http.Request) bool) (*docker.Client, func() []string) {
	t.Helper()
	return faultyDockerAt(t, "1.44", handle)
}

// faultyDockerAt 和 faultyDocker 一样，只是接口版本是 api。
func faultyDockerAt(t *testing.T, api string, handle func(w http.ResponseWriter, r *http.Request) bool) (*docker.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			fmt.Fprintf(w, `{"Version":"29.0.0","ApiVersion":%q}`, api)
			return
		}
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if !handle(w, r) {
			http.Error(w, `{"message":"模拟的 Docker 内部错误"}`, http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	dc, err := docker.New(context.Background(), docker.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	return dc, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

// prepareWith 用要写入目录 shared 的容器 new-app（和一个不用数据的 kept）按迁入的顺序调用
// decide、begin、prepare。新服务器上的已有容器由 handle 回答（shared 是那个目录的完整路径）。
// 返回屏幕上的输出（失败时有原因）和停过的容器。
func prepareWith(t *testing.T, d preflight.Decisions, handle func(w http.ResponseWriter, p, shared string) bool) (string, []string) {
	t.Helper()
	home := t.TempDir()
	shared := filepath.Join(home, "shared")
	var stopped []string
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
		p := strings.TrimPrefix(req.URL.Path, "/v1.44")
		if strings.HasSuffix(p, "/stop") {
			stopped = append(stopped, p)
			w.WriteHeader(204)
			return true
		}
		return handle(w, p, shared)
	})
	data := filepath.Join(home, "unpacked")
	os.MkdirAll(filepath.Join(data, "containers"), 0o700)
	os.WriteFile(filepath.Join(data, "containers", "new-app.json"),
		[]byte(`{"Id":"source","Name":"/new-app","Config":{"Image":"alpine"},"Mounts":[{"Type":"bind","Source":"`+shared+`","Destination":"/data"}]}`), 0o600)
	os.WriteFile(filepath.Join(data, "containers", "kept.json"), []byte(`{"Id":"source-kept","Name":"/kept","Config":{"Image":"alpine"}}`), 0o600)
	var out bytes.Buffer
	screen := ui.New(&out, false, nil)
	r := &run{Deps: Deps{Docker: dc, Home: home, Screen: screen, Getenv: func(string) string { return "" }}, d: d,
		plan:    &bundle.Plan{Containers: []bundle.PlanContainer{{Name: "kept"}, {Name: "new-app"}}},
		m:       &bundle.Manifest{Containers: []bundle.Container{{Name: "kept"}, {Name: "new-app"}}, Binds: []bundle.Bind{{Path: shared}}},
		dataDir: data, rbDir: filepath.Join(home, "rollback", "current"), anonVolumes: map[string]bundle.Volume{},
		undo: &undo{mountPoint: noMounts, dc: dc, screen: screen, logf: t.Logf, fail: func(string) bool { return false }}}
	err := r.decide(context.Background())
	if err == nil {
		err = r.begin()
	}
	if err == nil {
		err = r.prepare(context.Background())
	}
	if r.j != nil {
		r.j.close()
	}
	if err == nil {
		return "", stopped
	}
	return out.String(), stopped
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
	t.Cleanup(func() { log.Finish(logging.Failed, true) })
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
