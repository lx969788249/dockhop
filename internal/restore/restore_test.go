package restore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
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
