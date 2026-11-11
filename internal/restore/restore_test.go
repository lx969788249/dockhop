package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/docker/dockertest"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 检查环境时按 Ctrl+C：还没改动新服务器，按“已取消”处理，不显示被打断的那一步“失败”了。
func TestCancelWhileChecking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := dockertest.New(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/info") {
			cancel() // 读取 Docker 信息时用户按了 Ctrl+C
			<-r.Context().Done()
			return true
		}
		return false
	})
	dc := fake.Client()
	link, _ := crypto.NewLink("127.0.0.1", 1)
	var out bytes.Buffer
	var counted []string
	err := Run(ctx, Deps{Docker: dc, Screen: ui.New(&out, false, nil).WithContext(ctx), Home: t.TempDir(),
		Getenv:    func(string) string { return "" },
		Succeeded: func() { counted = append(counted, "成功") }, Failed: func() { counted = append(counted, "失败") },
	}, Options{Link: link, Yes: true})
	if !errors.Is(err, ui.ErrCancelled) || !strings.Contains(out.String(), "已取消，这台服务器上什么都没有改动") || strings.Contains(out.String(), "✗") {
		t.Fatalf("应按已取消处理、不显示失败：%v\n%s", err, out.String())
	}
	if len(counted) > 0 {
		t.Errorf("取消不算成功也不算失败：%v", counted)
	}
}

// 还没改动这台服务器就停下了（这里是读取 Docker 信息就失败）：照样显示迁入失败、留着日志，但不算一次失败
// （问题表 D10）；失败的次数只算动了手之后没成功的（见 TestEnding）。
func TestStopBeforeChangeNotCounted(t *testing.T) {
	fake := dockertest.New(t, func(http.ResponseWriter, *http.Request) bool { return false })
	dc := fake.Client() // 读取 Docker 信息就失败
	link, _ := crypto.NewLink("127.0.0.1", 1)
	var out bytes.Buffer
	var counted []string
	err := Run(context.Background(), Deps{Docker: dc, Screen: ui.New(&out, false, nil), Home: t.TempDir(),
		Getenv:    func(string) string { return "" },
		Succeeded: func() { counted = append(counted, "成功") }, Failed: func() { counted = append(counted, "失败") },
	}, Options{Link: link, Yes: true})
	if !errors.Is(err, ui.ErrReported) || !strings.Contains(out.String(), "❌ 迁入失败") || len(counted) > 0 {
		t.Fatalf("应显示失败，不算成功也不算失败：%v %v\n%s", err, counted, out.String())
	}
}

// 迁入的结局：结果摘要按它写，迁移次数按它统计。已经提交、只是清理没做完的，服务和数据都已迁入，
// 算成功；只迁入了一部分、改动之后失败的算失败；还没改动就取消或者停下、改动之后按 Ctrl+C 中断的
// 不算成功也不算失败。
func TestEnding(t *testing.T) {
	bg := context.Background()
	stopped, cancel := context.WithCancel(bg)
	cancel()
	// state 是这次迁入走到了哪里：开始了事务（began）、提交了、只保留了一部分、删过上次留下的内容。
	type state struct{ began, committed, partial, rollbackDone bool }
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		st   state
		want ending
	}{
		{"迁入完成", bg, nil, state{began: true, committed: true}, endSucceeded},
		{"已经提交，清理没做完", bg, ui.ErrReported, state{began: true, committed: true}, endCommitted},
		{"只迁入了一部分", bg, errPartial, state{began: true, committed: true, partial: true}, endPartial},
		{"还没改动就停下（检查发现问题、动手前核对发现变了）", bg, ui.ErrReported, state{}, endStopped},
		{"改动之后失败", bg, ui.ErrReported, state{began: true}, endFailed},
		{"还没改动就按了 Ctrl+C", stopped, ui.ErrReported, state{}, endCancelled},
		{"回答问题时按了 Ctrl+D", bg, io.EOF, state{}, endCancelled},
		{"改动之后按了 Ctrl+C", stopped, ui.ErrReported, state{began: true}, endInterrupted},
		{"删除上次留下的内容之后按了 Ctrl+C", stopped, ui.ErrReported, state{rollbackDone: true}, endInterrupted},
	}
	for _, c := range cases {
		r := testRun(t, Deps{}, Options{})
		if c.st.began {
			r.j = &Journal{}
		}
		r.out.committed, r.out.partial, r.out.rollbackDone = c.st.committed, c.st.partial, c.st.rollbackDone
		if got := r.ending(c.ctx, c.err); got != c.want {
			t.Errorf("%s：%v，应为 %v", c.name, got, c.want)
		}
	}
}

// 删除上次被强制结束的迁入留下的内容时按 Ctrl+C：和这次迁入失败时的撤销一样做完，不能停在
// 一半；也不能说成“什么都没有改动”、当作普通的取消（那样日志会被删掉）。删不掉的地方列在
// 结果里，回滚资料留着，下次运行迁入时接着删。
func TestCancelDuringRecovery(t *testing.T) {
	for _, stuck := range []bool{false, true} {
		t.Run(fmt.Sprintf("删不掉=%v", stuck), func(t *testing.T) {
			t.Setenv("PATH", t.TempDir()) // 不调用这台机器上真的 docker compose
			home := t.TempDir()
			dir := filepath.Join(home, "rollback", "current")
			j, err := openJournal(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"new-a", "new-b"} {
				for _, e := range []Entry{{Op: opContainer, Name: name}, {Op: opCreated, Name: name, ID: name}} {
					if err := j.add(e); err != nil {
						t.Fatal(err)
					}
				}
			}
			j.close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var deleted []string
			fake := dockertest.New(t, func(w http.ResponseWriter, req *http.Request) bool {
				p := req.URL.Path
				switch {
				case p == "/info":
					io.WriteString(w, `{"ServerVersion":"29.0.0"}`)
				case p == "/containers/json":
					io.WriteString(w, `[]`)
				case req.Method == http.MethodGet && strings.HasSuffix(p, "/json"):
					name := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
					fmt.Fprintf(w, `{"Id":%q,"Name":"/%s","State":{"Status":"running","Running":true}}`, name, name)
				case req.Method == http.MethodDelete:
					cancel() // 删第一个容器时用户按了 Ctrl+C
					if stuck && p == "/containers/new-a" {
						return false
					}
					deleted = append(deleted, strings.TrimPrefix(p, "/containers/"))
					w.WriteHeader(http.StatusNoContent)
				default:
					return false
				}
				return true
			})
			dc := fake.Client()
			link, _ := crypto.NewLink("127.0.0.1", 1)
			var out bytes.Buffer
			err = Run(ctx, Deps{Docker: dc, Screen: ui.New(&out, false, nil).WithContext(ctx), Home: home,
				Getenv: func(string) string { return "" }}, Options{Link: link, Yes: true})
			_, statErr := os.Stat(journalPath(dir))
			text := out.String()
			if errors.Is(err, ui.ErrCancelled) || !errors.Is(err, ui.ErrReported) || strings.Contains(text, "什么都没有改动") {
				t.Fatalf("改动过这台服务器，不能按普通的取消处理：%v\n%s", err, text)
			}
			if !stuck {
				if strings.Join(deleted, ",") != "new-b,new-a" || statErr == nil || !strings.Contains(text, "迁入的内容已全部删除") {
					t.Fatalf("按了 Ctrl+C 也要删完（删了 %v，日志还在：%v）：\n%s", deleted, statErr == nil, text)
				}
				return
			}
			if statErr != nil || !strings.Contains(text, "迁入的内容没能全部删除") || !strings.Contains(text, "new-a 没能删除") ||
				!strings.Contains(text, "回滚资料："+dir) {
				t.Fatalf("删不掉的要列在结果里，回滚资料留着：\n%s", text)
			}
		})
	}
}

// 迁出正在运行（拿着 out.lock）时，它的临时容器在读数据卷：迁入不能把它当作遗留的删掉。
// 这里是老版本的临时容器（没记是谁的）；旧服务器连不上，迁入在下载之前就结束。
func TestRestoreKeepsRunningExportHelper(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // 不调用这台机器上真的 docker compose
	home := t.TempDir()
	lock, err := host.TryLock(filepath.Join(home, "locks", "out.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	var deleted []string
	fake := dockertest.New(t, func(w http.ResponseWriter, req *http.Request) bool {
		switch {
		case strings.HasSuffix(req.URL.Path, "/containers/json"):
			io.WriteString(w, `[{"Id":"export-helper","Names":["/dockhop-helper-x-1"],"Labels":{"dockhop.helper":"1"}}]`)
		case req.Method == http.MethodDelete:
			deleted = append(deleted, req.URL.Path)
			w.WriteHeader(204)
		default:
			return false
		}
		return true
	})
	dc := fake.Client()
	link, _ := crypto.NewLink("127.0.0.1", 1)
	var out bytes.Buffer
	Run(context.Background(), Deps{Docker: dc, Home: home, Screen: ui.New(&out, false, nil), Prompt: ui.NewPrompter(nil, &out),
		Getenv: func(string) string { return "" }}, Options{Link: link})
	if len(deleted) > 0 {
		t.Errorf("删除了 %v", deleted)
	}
}

// 不经过命令行调用迁入时可以不给 MountPoint，按这台机器的实际情况判断（见 host.IsMountPoint），
// 不能在换目录时崩溃（问题表 D14）。挂载信息只有 Linux 上有，别的系统上只看不崩溃。
func TestRunDefaultsMountPoint(t *testing.T) {
	r := newRun(Deps{Home: t.TempDir()}, Options{})
	if _, err := r.newDirSwap(t.TempDir(), true); err != nil && runtime.GOOS == "linux" {
		t.Fatal(err)
	}
}
