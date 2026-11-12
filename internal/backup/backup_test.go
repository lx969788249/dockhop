package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lx969788249/dockhop/internal/docker"
	"github.com/lx969788249/dockhop/internal/docker/dockertest"
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
	fake := dockertest.New(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.HasSuffix(r.URL.Path, "/info"):
			cancel() // 读取 Docker 信息时用户按了 Ctrl+C
			<-r.Context().Done()
		default:
			http.Error(w, `{"message":"x"}`, http.StatusInternalServerError)
		}
		return true
	})
	dc := fake.Client()
	var out bytes.Buffer
	failed := 0
	err := Run(ctx, Deps{Docker: dc, Screen: ui.New(&out, false, nil).WithContext(ctx), Home: t.TempDir(),
		Failed: func() { failed++ }}, Options{})
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
	fake := dockertest.New(t, func(w http.ResponseWriter, r *http.Request) bool {
		http.Error(w, `{"message":"x"}`, http.StatusInternalServerError) // 读取 Docker 信息就失败
		return true
	})
	dc := fake.Client()
	var out bytes.Buffer
	failed := 0
	err := Run(context.Background(), Deps{Docker: dc, Screen: ui.New(&out, false, nil), Home: t.TempDir(),
		Failed: func() { failed++ }}, Options{})
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
	// state 是这次迁出走到了哪里：开始了传输（serving）、新服务器报告的结果、停下了几个容器、恢复了几个。
	type state struct {
		serving             bool
		status              string
		toRestore, restored int
	}
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		st   state
		want ending
	}{
		{"新服务器迁入成功", bg, nil, state{serving: true, status: "success"}, endSucceeded},
		{"新服务器迁入失败后结束传输", bg, nil, state{serving: true, status: "failed"}, endSucceeded},
		{"传输时按 Ctrl+C 结束", stopped, nil, state{serving: true}, endSucceeded},
		{"还没改动就停下（检查发现问题、停机前核对发现变了）", bg, ui.ErrReported, state{}, endStopped},
		{"停机打包之后失败", bg, ui.ErrReported, state{toRestore: 2, restored: 2}, endFailed},
		{"停下的容器没能全部恢复", bg, nil, state{toRestore: 2, restored: 1}, endFailed},
		{"还没改动就按了 Ctrl+C", stopped, ui.ErrReported, state{}, endCancelled},
		{"回答问题时按了 Ctrl+D", bg, io.EOF, state{}, endCancelled},
		{"停机打包时按了 Ctrl+C", stopped, ui.ErrReported, state{toRestore: 2, restored: 2}, endInterrupted},
	}
	for _, c := range cases {
		r := testRun(t, Deps{}, Options{})
		if c.st.serving {
			r.server = serving
		}
		r.status, r.toRestore, r.restored = c.st.status, c.st.toRestore, c.st.restored
		if got := r.ending(c.ctx, c.err); got != c.want {
			t.Errorf("%s：%v，应为 %v", c.name, got, c.want)
		}
	}
}

// 迁出停下时，建议单独写在建议里（结果摘要的“建议：”一行），不接在原因后面：以前磁盘可能不够、端口都被占用、
// 容器停不下、停下的容器恢复不了这四处，把建议当成了出错的细节和原因连成一句，结果摘要里没有建议。
func TestAdviceIsHint(t *testing.T) {
	hintApart(t, "磁盘可能不够", outSteps(t, 1<<60, 0))
	if ln, err := net.Listen("tcp", ":65535"); err == nil { // 占不上是别的程序在用，一样
		defer ln.Close()
	}
	hintApart(t, "端口都被占用", outSteps(t, 10, 65535))

	fake := dockertest.New(t, func(http.ResponseWriter, *http.Request) bool { return false }) // 停止、启动都失败
	r := testRun(t, Deps{Docker: fake.Client()}, Options{})
	web := &docker.Container{ID: "web-id", Name: "/web"}
	web.State.Running = true
	hintApart(t, "容器停不下", r.stopSources(context.Background(), &collected{sel: &selection{items: []*item{{c: web}}}}))
	r.stopped = []StoppedContainer{{Name: "web", ID: "web-id", State: "running"}}
	hintApart(t, "停下的容器恢复不了", r.resumeSources(context.Background(), time.Now()))
}

// hintApart 确认 err 是带建议的 *ui.Failure，建议没有接在原因里。
func hintApart(t *testing.T, what string, err error) {
	t.Helper()
	var f *ui.Failure
	if !errors.As(err, &f) || f.Hint == "" || strings.Contains(f.Error(), f.Hint) {
		t.Errorf("%s：建议应单独写在建议里：%v（建议：%q）", what, err, ui.HintOf(err))
	}
}

// outSteps 用只有一个容器 web（挂着数据卷 data，大小 size）的假 Docker 按步骤迁出 web，--port 是 port；
// 返回在提供下载之前停下时出的错。
func outSteps(t *testing.T, size int64, port int) error {
	t.Helper()
	fake := dockertest.New(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/info":
			io.WriteString(w, `{"ServerVersion":"27.5.1"}`)
		case "/containers/json":
			io.WriteString(w, `[{"Id":"web-id","Names":["/web"],"State":"running"}]`)
		case "/containers/web-id/json":
			io.WriteString(w, `{"Id":"web-id","Name":"/web","Image":"sha256:img","Config":{"Image":"alpine"},"State":{"Status":"running","Running":true},`+
				`"Mounts":[{"Type":"volume","Name":"data","Destination":"/data","RW":true}]}`)
		case "/system/df":
			fmt.Fprintf(w, `{"Volumes":[{"Name":"data","UsageData":{"Size":%d}}]}`, size)
		case "/images/sha256:img/json":
			io.WriteString(w, `{"Id":"sha256:img","Size":100}`)
		case "/volumes/data":
			io.WriteString(w, `{"Name":"data","Driver":"local"}`)
		default:
			return false
		}
		return true
	})
	return testRun(t, Deps{Docker: fake.Client()}, Options{Include: []string{"web"}, Port: port}).steps(context.Background())
}
