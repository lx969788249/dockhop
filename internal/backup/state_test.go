package backup

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/ui"
)

// fakeDocker 是只认识几个接口的假 Docker：容器都是停着的，记下启动了哪些。
func fakeDocker(t *testing.T) (*docker.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var started []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/version":
			w.Write([]byte(`{"Version":"29.0.0","ApiVersion":"1.44"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			w.Write([]byte(`{"Id":"abc","Name":"/web","State":{"Status":"exited","Running":false}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			mu.Lock()
			started = append(started, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
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
		return append([]string(nil), started...)
	}
}

// 上次迁出被强制结束后，记录里的进程号被别的进程占用了（例如服务器重启后），
// 也要恢复停下的容器；只有那次迁出真的还在运行（拿着锁）时才跳过。
func TestRecoverInterruptedUsesLock(t *testing.T) {
	home := t.TempDir()
	write := func() {
		if err := writeStopped(home, []StoppedContainer{{Name: "web", ID: "abc", State: "running"}}); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	screen := ui.New(&out, false, nil)
	prompt := ui.NewPrompter(nil, &out)

	// 那次迁出还在运行：拿着锁。不能动它停下的容器。
	write()
	dc, started := fakeDocker(t)
	lock, err := host.TryLock(lockPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if err := RecoverInterrupted(context.Background(), dc, home, screen, prompt); err != nil {
		t.Fatal(err)
	}
	lock.Unlock()
	if len(started()) != 0 {
		t.Fatalf("迁出还在运行时不应启动容器：%v", started())
	}
	if _, err := os.Stat(statePath(home)); err != nil {
		t.Fatal("迁出还在运行时不应删除停机记录")
	}

	// 那次迁出已经结束，记录里的进程号现在是一个正在运行的别的进程（这里用父进程）。
	b, _ := os.ReadFile(statePath(home))
	b = bytes.Replace(b, []byte(`"pid": `+strconv.Itoa(os.Getpid())), []byte(`"pid": `+strconv.Itoa(os.Getppid())), 1)
	if !bytes.Contains(b, []byte(`"pid": `+strconv.Itoa(os.Getppid()))) {
		t.Fatalf("没改成父进程的进程号：%s", b)
	}
	os.WriteFile(statePath(home), b, 0o600)
	if err := RecoverInterrupted(context.Background(), dc, home, screen, prompt); err != nil {
		t.Fatal(err)
	}
	if len(started()) != 1 {
		t.Fatalf("应恢复停下的容器：%v\n%s", started(), out.String())
	}
	if _, err := os.Stat(statePath(home)); !os.IsNotExist(err) {
		t.Error("恢复后应删除停机记录")
	}
}
