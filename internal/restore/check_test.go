package restore

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
