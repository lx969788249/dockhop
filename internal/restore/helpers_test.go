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

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/docker/dockertest"
	"github.com/lx969788249/dockhop/internal/logging"
	"github.com/lx969788249/dockhop/internal/preflight"
	"github.com/lx969788249/dockhop/internal/spec"
	"github.com/lx969788249/dockhop/internal/ui"
)

// noMounts 是没有任何单独挂载的目录时的 Deps.MountPoint。
func noMounts(string) (bool, error) { return false, nil }

// testRun 是测试用的 run，和迁入时一样用 newRun 造；没给的依赖补上测试用的：屏幕丢掉输出、环境变量都是空的、
// DockHop 的目录放在临时目录里、没有单独挂载的目录。迁入走到哪一步才有的内容（迁移计划、清单、要迁的容器……）
// 由测试自己填。
func testRun(t *testing.T, d Deps, o Options) *run {
	t.Helper()
	if d.Screen == nil {
		d.Screen = ui.New(io.Discard, false, nil)
	}
	if d.Getenv == nil {
		d.Getenv = func(string) string { return "" }
	}
	if d.Home == "" {
		d.Home = t.TempDir()
	}
	if d.MountPoint == nil {
		d.MountPoint = noMounts
	}
	return newRun(d, o)
}

// testMigrate 是测试用的、开始改动之后的 migrate：run 和 testRun 一样；前几步的结果（检查、迁移包、迁入范围）
// 都是空的，由测试自己填；没有事务日志，要的话测试自己开。
func testMigrate(t *testing.T, d Deps, o Options) *migrate {
	t.Helper()
	return &migrate{run: testRun(t, d, o), chk: &checked{}, pkg: &bundleFiles{},
		sc:      &scope{targets: map[string]bool{}, anonVolumes: map[string]bundle.Volume{}},
		imageAs: map[string]string{}, noted: map[string]bool{}}
}

// testUndo 是测试用的 undo：日志写到 t.Logf，不模拟失败，没有单独挂载的目录；screen 为空时丢掉输出。
// 别的按需要再填。
func testUndo(t *testing.T, dc *docker.Client, screen *ui.Screen) *undo {
	if screen == nil {
		screen = ui.New(io.Discard, false, nil)
	}
	return &undo{dc: dc, screen: screen, logf: t.Logf, fail: func(string) bool { return false }, mountPoint: noMounts,
		defaults: &spec.SourceDefaults{}}
}

// prepareWith 用要写入目录 shared 的容器 new-app（和一个不用数据的 kept）按迁入的顺序调用
// decide、begin、prepare（见 decideThenPrepare）。新服务器上的已有容器由 handle 回答（shared 是那个目录的完整路径）。
// 返回停过的容器和出的错。
func prepareWith(t *testing.T, d preflight.Decisions, handle func(w http.ResponseWriter, p, shared string) bool) ([]string, error) {
	t.Helper()
	home := t.TempDir()
	shared := filepath.Join(home, "shared")
	var stopped []string
	fake := dockertest.New(t, func(w http.ResponseWriter, req *http.Request) bool {
		p := req.URL.Path
		if strings.HasSuffix(p, "/stop") {
			stopped = append(stopped, p)
			w.WriteHeader(204)
			return true
		}
		return handle(w, p, shared)
	})
	dc := fake.Client()
	data := filepath.Join(home, "unpacked")
	os.MkdirAll(filepath.Join(data, "containers"), 0o700)
	os.WriteFile(filepath.Join(data, "containers", "new-app.json"),
		[]byte(`{"Id":"source","Name":"/new-app","Config":{"Image":"alpine"},"Mounts":[{"Type":"bind","Source":"`+shared+`","Destination":"/data"}]}`), 0o600)
	os.WriteFile(filepath.Join(data, "containers", "kept.json"), []byte(`{"Id":"source-kept","Name":"/kept","Config":{"Image":"alpine"}}`), 0o600)
	r := testRun(t, Deps{Docker: dc, Home: home}, Options{})
	chk := &checked{d: d, plan: &bundle.Plan{Containers: []bundle.PlanContainer{{Name: "kept"}, {Name: "new-app", Binds: []string{shared}}},
		Binds: []bundle.PlanBind{{Path: shared}}}}
	pkg := &bundleFiles{dataDir: data,
		m: &bundle.Manifest{Containers: []bundle.Container{{Name: "kept"}, {Name: "new-app"}}, Binds: []bundle.Bind{{Path: shared}}}}
	_, err := decideThenPrepare(r, chk, pkg)
	return stopped, err
}

// decideThenPrepare 按迁入的顺序调用 decide、begin、prepare，最后关掉事务日志。返回开始改动之后的 migrate
// （begin 之前就出错时为空）和出的错。
func decideThenPrepare(r *run, chk *checked, pkg *bundleFiles) (*migrate, error) {
	ctx := context.Background()
	sc, err := r.decide(ctx, chk, pkg)
	if err != nil {
		return nil, err
	}
	mg, err := r.begin(nil, chk, pkg, sc)
	if err != nil {
		return nil, err
	}
	defer mg.j.close()
	return mg, mg.prepare(ctx)
}

// 假 Docker 上的一个容器：每次查询按顺序给出下一个样子，最后一个样子一直保持。
func fakeContainer(t *testing.T, logs string, states ...string) (*docker.Client, *bytes.Buffer, *migrate) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	fake := dockertest.New(t, func(w http.ResponseWriter, req *http.Request) bool {
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
	dc := fake.Client()
	root := t.TempDir()
	log, err := logging.Open(filepath.Join(root, "logs"), "in", &logging.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Finish(logging.Failed, true) })
	var out bytes.Buffer
	r := testMigrate(t, Deps{Docker: dc, Screen: ui.New(&out, false, nil), Log: log, Redactor: &logging.Redactor{},
		Getenv: func(k string) string {
			if k == "DOCKHOP_TEST_STARTUP_GRACE" {
				return "1s"
			}
			return ""
		}}, Options{})
	return dc, &out, r
}
