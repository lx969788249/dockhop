package backup

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// Docker 临时出错、读不到停下的容器时，不能当作容器已经删掉、把停机记录删了。
func TestRecoverInterruptedKeepsRecordOnDockerError(t *testing.T) {
	home := t.TempDir()
	if err := writeStopped(home, []StoppedContainer{{Name: "web", ID: "abc", State: "running"}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Write([]byte(`{"Version":"29.0.0","ApiVersion":"1.44"}`))
			return
		}
		http.Error(w, `{"message":"模拟的 Docker 内部错误"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()
	dc, err := docker.New(context.Background(), docker.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = RecoverInterrupted(context.Background(), dc, home, ui.New(&out, false, nil), ui.NewPrompter(nil, &out))
	if err == nil || !strings.Contains(err.Error(), "停机记录保留") {
		t.Fatalf("应报错：%v", err)
	}
	if _, err := os.Stat(statePath(home)); err != nil {
		t.Fatal("停机记录应保留")
	}
}

// 中断恢复按记录里原来的状态判断：原来暂停、现在却在运行的（停机打包时恢复运行之后、或者恢复时
// 启动之后被强制结束）要再暂停；已经是原来状态的不动。恢复不了时保留记录，下次接着恢复。
func TestRecoverInterruptedStates(t *testing.T) {
	cases := []struct {
		name, recorded, now string
		pauseOK             bool
		want                string // 收到的操作
		keepRecord          bool
	}{
		{"原来运行、现在停着", "running", "exited", true, "start", false},
		{"原来运行、现在在运行", "running", "running", true, "", false},
		{"原来暂停、现在停着", "paused", "exited", true, "start pause", false},
		{"原来暂停、现在在运行", "paused", "running", true, "pause", false},
		{"原来暂停、现在暂停", "paused", "paused", true, "", false},
		{"原来暂停、现在在运行、暂停失败", "paused", "running", false, "pause", true},
	}
	states := map[string]string{
		"exited":  `"Status":"exited","Running":false`,
		"running": `"Status":"running","Running":true`,
		"paused":  `"Status":"paused","Running":true,"Paused":true`,
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			if err := writeStopped(home, []StoppedContainer{{Name: "web", ID: "abc", State: c.recorded}}); err != nil {
				t.Fatal(err)
			}
			now := c.now
			var calls []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/version":
					w.Write([]byte(`{"Version":"29.0.0","ApiVersion":"1.44"}`))
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
					w.Write([]byte(`{"Id":"abc","Name":"/web","State":{` + states[now] + `}}`))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
					calls, now = append(calls, "start"), "running"
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pause"):
					calls = append(calls, "pause")
					if !c.pauseOK {
						http.Error(w, `{"message":"模拟暂停失败"}`, http.StatusInternalServerError)
						return
					}
					now = "paused"
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			dc, err := docker.New(context.Background(), docker.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")))
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err = RecoverInterrupted(context.Background(), dc, home, ui.New(&out, false, nil), ui.NewPrompter(nil, &out))
			if got := strings.Join(calls, " "); got != c.want {
				t.Errorf("操作：%q，应该：%q", got, c.want)
			}
			_, statErr := os.Stat(statePath(home))
			if kept := statErr == nil; kept != c.keepRecord || (err != nil) != c.keepRecord {
				t.Errorf("记录保留：%v，应该：%v（错误：%v）", kept, c.keepRecord, err)
			}
		})
	}
}

// 停机记录读不懂时（例如更早的版本写到一半断了电）不能删掉：上次停下的容器可能还停着，删了
// 就没人管了。保留记录，说明怎么处理。
func TestRecoverInterruptedKeepsBrokenRecord(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Dir(statePath(home)), 0o700)
	os.WriteFile(statePath(home), []byte(`{"pid": 12, "containers": [{"na`), 0o600)
	dc, started := fakeDocker(t)
	var out bytes.Buffer
	err := RecoverInterrupted(context.Background(), dc, home, ui.New(&out, false, nil), ui.NewPrompter(nil, &out))
	if err == nil || !strings.Contains(err.Error(), "docker ps -a") {
		t.Fatalf("应停下并说明怎么处理：%v", err)
	}
	if _, serr := os.Stat(statePath(home)); serr != nil {
		t.Error("读不懂的停机记录应保留")
	}
	if len(started()) != 0 {
		t.Errorf("不应启动任何容器：%v", started())
	}
}

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
	err = Run(ctx, Deps{Docker: dc, Screen: ui.New(&out, false, nil).WithContext(ctx), Home: t.TempDir(),
		Getenv: func(string) string { return "" }}, Options{})
	if !errors.Is(err, ui.ErrCancelled) || !strings.Contains(out.String(), "已取消，这台服务器上什么都没有改动") || strings.Contains(out.String(), "✗") {
		t.Fatalf("应按已取消处理、不显示失败：%v\n%s", err, out.String())
	}
}
