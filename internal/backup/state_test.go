package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/docker/dockertest"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/ui"
)

// stoppedDocker 是容器都停着的假 Docker（见 dockertest）；返回的函数列出启动过哪些容器。
func stoppedDocker(t *testing.T) (*docker.Client, func() []string) {
	t.Helper()
	fake := dockertest.New(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			io.WriteString(w, `{"Id":"abc","Name":"/web","State":{"Status":"exited","Running":false}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
		return true
	})
	return fake.Client(), func() []string {
		var started []string
		for _, c := range fake.Calls() {
			if strings.HasSuffix(c, "/start") {
				started = append(started, c)
			}
		}
		return started
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
	dc, started := stoppedDocker(t)
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
	fake := dockertest.New(t, func(w http.ResponseWriter, r *http.Request) bool {
		http.Error(w, `{"message":"模拟的 Docker 内部错误"}`, http.StatusInternalServerError)
		return true
	})
	dc := fake.Client()
	var out bytes.Buffer
	err := RecoverInterrupted(context.Background(), dc, home, ui.New(&out, false, nil), ui.NewPrompter(nil, &out))
	var de *docker.Error
	if !errors.As(err, &de) || de.Status != http.StatusInternalServerError {
		t.Fatalf("应报出 Docker 的错误：%v", err)
	}
	if _, err := os.Stat(statePath(home)); err != nil {
		t.Fatal("停机记录应保留")
	}
}

// 恢复上次停下的容器时按 Ctrl+C：照样全部恢复，不能让剩下的一直停着。
func TestRecoverInterruptedFinishesWhenCancelled(t *testing.T) {
	home := t.TempDir()
	if err := writeStopped(home, []StoppedContainer{{Name: "db", ID: "db", State: "running"}, {Name: "web", ID: "web", State: "running"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	running := map[string]bool{}
	fake := dockertest.New(t, func(w http.ResponseWriter, r *http.Request) bool {
		id := strings.Split(strings.TrimPrefix(r.URL.Path, "/containers/"), "/")[0]
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			w.Write([]byte(`{"Id":"` + id + `","Name":"/` + id + `","State":{"Running":` + strconv.FormatBool(running[id]) + `}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			cancel() // 启动第一个容器时用户按了 Ctrl+C
			running[id] = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
		return true
	})
	dc := fake.Client()
	var out bytes.Buffer
	if err := RecoverInterrupted(ctx, dc, home, ui.New(&out, false, nil), ui.NewPrompter(nil, &out)); err != nil {
		t.Fatalf("应全部恢复：%v\n%s", err, out.String())
	}
	if !running["db"] || !running["web"] {
		t.Fatalf("两个容器都要启动：%v", running)
	}
	if _, err := os.Stat(statePath(home)); !os.IsNotExist(err) {
		t.Fatal("全部恢复后停机记录应删除")
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
			fake := dockertest.New(t, func(w http.ResponseWriter, r *http.Request) bool {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
					w.Write([]byte(`{"Id":"abc","Name":"/web","State":{` + states[now] + `}}`))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
					calls, now = append(calls, "start"), "running"
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pause"):
					calls = append(calls, "pause")
					if !c.pauseOK {
						http.Error(w, `{"message":"模拟暂停失败"}`, http.StatusInternalServerError)
						return true
					}
					now = "paused"
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
				return true
			})
			dc := fake.Client()
			var out bytes.Buffer
			err := RecoverInterrupted(context.Background(), dc, home, ui.New(&out, false, nil), ui.NewPrompter(nil, &out))
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
	dc, started := stoppedDocker(t)
	var out bytes.Buffer
	err := RecoverInterrupted(context.Background(), dc, home, ui.New(&out, false, nil), ui.NewPrompter(nil, &out))
	var se *json.SyntaxError
	if !errors.As(err, &se) {
		t.Fatalf("记录读不懂时应停下：%v", err)
	}
	if _, serr := os.Stat(statePath(home)); serr != nil {
		t.Error("读不懂的停机记录应保留")
	}
	if len(started()) != 0 {
		t.Errorf("不应启动任何容器：%v", started())
	}
}
