package restore

import (
	"bytes"
	"context"
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
	"github.com/lx969788249/dockhop/internal/logging"
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
	r := &run{Deps: Deps{Screen: ui.New(&out, false, nil), Log: log, Getenv: func(string) string { return "" }}, opts: Options{Link: link}}
	if err := r.fetchPlan(context.Background()); err == nil || r.plan != nil || !strings.Contains(out.String(), "没有提供迁移计划") {
		t.Fatalf("没有迁移计划时应拒绝迁入：%v\n%s", err, out.String())
	}
}

// 迁入前检查读新服务器的情况时按 Ctrl+C：马上停下，当作取消（以前这一段不响应 Ctrl+C，要等 Docker
// 回话；被取消了也报成“读取新服务器的情况失败”）。
func TestCheckStopsOnCancel(t *testing.T) {
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
		if strings.HasSuffix(req.URL.Path, "/info") {
			select {
			case <-req.Context().Done():
			case <-time.After(3 * time.Second):
			}
		}
		return false
	})
	dir := t.TempDir()
	log, err := logging.Open(filepath.Join(dir, "logs"), "in", &logging.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Finish(logging.Failed, true)
	r := &run{Deps: Deps{Docker: dc, Screen: ui.New(io.Discard, false, nil), Prompt: ui.NewPrompter(nil, io.Discard), Log: log,
		Home: dir, Getenv: func(string) string { return "" }}, plan: &bundle.Plan{}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if _, err := r.check(ctx); !ui.Canceled(err) || time.Since(start) > 2*time.Second {
		t.Fatalf("应马上停下、当作取消：%v（用了 %s）", err, time.Since(start))
	}
}
