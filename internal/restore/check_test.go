package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lx969788249/dockhop/internal/bundle"
	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/docker/dockertest"
	"github.com/lx969788249/dockhop/internal/logging"
	"github.com/lx969788249/dockhop/internal/preflight"
	"github.com/lx969788249/dockhop/internal/transfer"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 旧服务器有迁移包、没有迁移计划：不能迁入。迁入前检查和核对下载的文件都靠它，
// 以前没有它时会跳过检查，直接替换同名的服务。
func TestRefusesWithoutPlan(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	link, _ := crypto.NewLink("127.0.0.1", ln.Addr().(*net.TCPAddr).Port)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bundle"), []byte("bundle"), 0o600)
	srv, err := transfer.NewServer(ln, link, filepath.Join(dir, "bundle"), "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	log, err := logging.Open(filepath.Join(dir, "logs"), "in", &logging.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Finish(logging.Failed, true)
	var out bytes.Buffer
	r := testRun(t, Deps{Screen: ui.New(&out, false, nil), Log: log}, Options{Link: link})
	if plan, err := r.fetchPlan(context.Background()); !errors.Is(err, transfer.ErrNoPlan) || plan != nil {
		t.Fatalf("没有迁移计划时应拒绝迁入：%v", err)
	}
}

// 版本不一样的迁移计划不能用（见 bundle.PlanVersion），在下载和改动之前停下：旧版本迁出的，说明要在旧服务器
// 上用新版本重新迁出；更新版本迁出的，说明要用新版本迁入。
func TestRefusesOtherPlanVersions(t *testing.T) {
	for v, want := range map[int]error{bundle.PlanVersion - 1: bundle.ErrPlanOlder, bundle.PlanVersion + 1: bundle.ErrPlanNewer} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		link, _ := crypto.NewLink("127.0.0.1", ln.Addr().(*net.TCPAddr).Port)
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "bundle"), []byte("bundle"), 0o600)
		pf, _ := os.Create(filepath.Join(dir, "plan"))
		if err := bundle.EncryptPlan(pf, link, &bundle.Plan{Version: v}); err != nil {
			t.Fatal(err)
		}
		pf.Close()
		srv, err := transfer.NewServer(ln, link, filepath.Join(dir, "bundle"), filepath.Join(dir, "plan"), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		go srv.Serve()
		var out bytes.Buffer
		r := testRun(t, Deps{Screen: ui.New(&out, false, nil), Log: &logging.Logger{}}, Options{Link: link})
		if plan, err := r.fetchPlan(context.Background()); !errors.Is(err, want) || plan != nil {
			t.Errorf("迁移计划的版本是 %d 时应拒绝（%v）：%v", v, want, err)
		}
		srv.Shutdown(context.Background())
	}
}

// 迁入前检查读新服务器的情况时按 Ctrl+C：马上停下，当作取消（以前这一段不响应 Ctrl+C，要等 Docker
// 回话；被取消了也报成“读取新服务器的情况失败”）。
func TestCheckStopsOnCancel(t *testing.T) {
	fake := dockertest.New(t, func(w http.ResponseWriter, req *http.Request) bool {
		if strings.HasSuffix(req.URL.Path, "/info") {
			select {
			case <-req.Context().Done():
			case <-time.After(3 * time.Second):
			}
		}
		return false
	})
	dc := fake.Client()
	dir := t.TempDir()
	log, err := logging.Open(filepath.Join(dir, "logs"), "in", &logging.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Finish(logging.Failed, true)
	r := testRun(t, Deps{Docker: dc, Prompt: ui.NewPrompter(nil, io.Discard), Log: log,
		Home: dir, Getenv: func(string) string { return "" }}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if _, err := r.check(ctx, &checked{plan: &bundle.Plan{}}, nil); !ui.Canceled(err) || time.Since(start) > 2*time.Second {
		t.Fatalf("应马上停下、当作取消：%v（用了 %s）", err, time.Since(start))
	}
}

// 只能在 Docker 所在的机器上做的事：写宿主机上的目录、放 Compose 项目文件，以及清空数据卷——替换
// 已有的卷、写进匿名卷（新建的匿名卷里有镜像带的文件）都要先清空，清空要把本机的程序放进临时容器。
// 只写进新建的有名字的卷不用清空。
func TestLocalWork(t *testing.T) {
	const anon = "3977206abb71f8aa4677d89b605e71e99ae3db6773688b6f62e61fbbadf9c34c"
	tg := &preflight.Target{Volumes: map[string]*docker.Volume{"data": {Name: "data"}}}
	for _, c := range []struct {
		name string
		w    preflight.Writes
		want string
	}{
		{"替换已有的卷", preflight.Writes{Volumes: []string{"data"}}, "替换已有的数据卷"},
		{"写进匿名卷", preflight.Writes{Volumes: []string{anon}}, "替换已有的数据卷"},
		{"新建的卷", preflight.Writes{Volumes: []string{"fresh"}}, ""},
		{"目录和项目", preflight.Writes{Binds: []string{"/srv/web"}, Projects: []bundle.PlanProject{{Name: "site"}}, Volumes: []string{"fresh"}}, "放目录 放 Compose 项目文件"},
	} {
		if got := strings.Join(localWork(c.w, tg), " "); got != c.want {
			t.Errorf("%s：%q，应为 %q", c.name, got, c.want)
		}
	}
}
