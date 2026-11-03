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
	"strings"
	"testing"

	"github.com/lx969788249/dockhop/internal/crypto"
	"github.com/lx969788249/dockhop/internal/host"
	"github.com/lx969788249/dockhop/internal/ui"
)

// 检查环境时按 Ctrl+C：还没改动新服务器，按“已取消”处理，不显示被打断的那一步“失败”了。
func TestCancelWhileChecking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/info") {
			cancel() // 读取 Docker 信息时用户按了 Ctrl+C
			<-r.Context().Done()
			return true
		}
		return false
	})
	link, _ := crypto.NewLink("127.0.0.1", 1)
	var out bytes.Buffer
	err := Run(ctx, Deps{Docker: dc, Screen: ui.New(&out, false, nil).WithContext(ctx), Home: t.TempDir(),
		Getenv: func(string) string { return "" }}, Options{Link: link, Yes: true})
	if !errors.Is(err, ui.ErrCancelled) || !strings.Contains(out.String(), "已取消，这台服务器上什么都没有改动") || strings.Contains(out.String(), "✗") {
		t.Fatalf("应按已取消处理、不显示失败：%v\n%s", err, out.String())
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
				if err := j.add(Entry{Op: opContainer, Name: name}); err != nil {
					t.Fatal(err)
				}
			}
			j.close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var deleted []string
			dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
				p := strings.TrimPrefix(req.URL.Path, "/v1.44")
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
	dc, _ := faultyDocker(t, func(w http.ResponseWriter, req *http.Request) bool {
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
	link, _ := crypto.NewLink("127.0.0.1", 1)
	var out bytes.Buffer
	Run(context.Background(), Deps{Docker: dc, Home: home, Screen: ui.New(&out, false, nil), Prompt: ui.NewPrompter(nil, &out),
		Getenv: func(string) string { return "" }}, Options{Link: link})
	if len(deleted) > 0 {
		t.Errorf("删除了 %v", deleted)
	}
}
