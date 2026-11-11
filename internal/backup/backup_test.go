package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/transfer"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 迁出拿到锁之后，以前被强制结束时留下的工作目录都是遗留的，删掉；迁入的不动。
func TestDropStaleOutWork(t *testing.T) {
	home := t.TempDir()
	for _, p := range []string{"work/out-old/bundle", "work/in-x/bundle"} {
		full := filepath.Join(home, p)
		os.MkdirAll(filepath.Dir(full), 0o700)
		os.WriteFile(full, []byte("x"), 0o600)
	}
	dropStale(home)
	if _, err := os.Stat(filepath.Join(home, "work", "out-old")); !os.IsNotExist(err) {
		t.Errorf("遗留的迁出工作目录应删掉：%v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "work", "in-x", "bundle")); err != nil {
		t.Errorf("迁入的不应动：%v", err)
	}
}

// 检查环境时按 Ctrl+C：还没改动任何东西，按“已取消”处理（日志、目录都不留），不显示被打断的
// 那一步“失败”了。被打断的那一步返回什么错误都有可能，看的是用户有没有按 Ctrl+C。
func TestCancelWhileChecking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/version":
			w.Write([]byte(`{"Version":"29.0.0","ApiVersion":"1.44"}`))
		case strings.HasSuffix(r.URL.Path, "/info"):
			cancel() // 读取 Docker 信息时用户按了 Ctrl+C
			<-r.Context().Done()
		default:
			http.Error(w, `{"message":"x"}`, http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	dc, err := docker.New(context.Background(), docker.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	failed := 0
	err = Run(ctx, Deps{Docker: dc, Screen: ui.New(&out, false, nil).WithContext(ctx), Home: t.TempDir(),
		Getenv: func(string) string { return "" }, Failed: func() { failed++ }}, Options{})
	if !errors.Is(err, ui.ErrCancelled) || !strings.Contains(out.String(), "已取消，这台服务器上什么都没有改动") || strings.Contains(out.String(), "✗") {
		t.Fatalf("应按已取消处理、不显示失败：%v\n%s", err, out.String())
	}
	if failed > 0 {
		t.Errorf("取消不算失败：%d", failed)
	}
}

// 还没改动这台服务器就停下了（这里是读取 Docker 信息就失败）：照样显示迁出失败、留着日志，但不算一次失败
// （问题表 D10）；失败的次数只算动了手之后没成功的（见 TestEnding）。
func TestStopBeforeChangeNotCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Write([]byte(`{"Version":"29.0.0","ApiVersion":"1.44"}`))
			return
		}
		http.Error(w, `{"message":"x"}`, http.StatusInternalServerError) // 读取 Docker 信息就失败
	}))
	defer srv.Close()
	dc, err := docker.New(context.Background(), docker.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	failed := 0
	err = Run(context.Background(), Deps{Docker: dc, Screen: ui.New(&out, false, nil), Home: t.TempDir(),
		Getenv: func(string) string { return "" }, Failed: func() { failed++ }}, Options{})
	if !errors.Is(err, ui.ErrReported) || !strings.Contains(out.String(), "❌ 迁出失败") || failed != 0 {
		t.Fatalf("应显示失败，不算一次失败（算了 %d 次）：%v\n%s", failed, err, out.String())
	}
}

// 迁出的结局：结果摘要按它写，失败的次数按它统计。传输开始之后用户自己结束传输都算正常结束
// （新服务器迁入失败时，失败算在新服务器那边）；停机打包之后失败、停下的容器没能全部恢复算失败；
// 还没改动就取消或者停下、停机打包时按 Ctrl+C 中断的不算失败。
func TestEnding(t *testing.T) {
	bg := context.Background()
	stopped, cancel := context.WithCancel(bg)
	cancel()
	serving := &transfer.Server{}
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		r    *run
		want ending
	}{
		{"新服务器迁入成功", bg, nil, &run{server: serving, status: "success"}, endSucceeded},
		{"新服务器迁入失败后结束传输", bg, nil, &run{server: serving, status: "failed"}, endSucceeded},
		{"传输时按 Ctrl+C 结束", stopped, nil, &run{server: serving}, endSucceeded},
		{"还没改动就停下（检查发现问题、停机前核对发现变了）", bg, ui.ErrReported, &run{}, endStopped},
		{"停机打包之后失败", bg, ui.ErrReported, &run{toRestore: 2, restored: 2}, endFailed},
		{"停下的容器没能全部恢复", bg, nil, &run{toRestore: 2, restored: 1}, endFailed},
		{"还没改动就按了 Ctrl+C", stopped, ui.ErrReported, &run{}, endCancelled},
		{"回答问题时按了 Ctrl+D", bg, io.EOF, &run{}, endCancelled},
		{"停机打包时按了 Ctrl+C", stopped, ui.ErrReported, &run{toRestore: 2, restored: 2}, endInterrupted},
	}
	for _, c := range cases {
		if got := c.r.ending(c.ctx, c.err); got != c.want {
			t.Errorf("%s：%v，应为 %v", c.name, got, c.want)
		}
	}
}
